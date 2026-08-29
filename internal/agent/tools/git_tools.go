package tools

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/proto"
)

const (
	// GitStatusToolName, GitDiffToolName, and GitLogToolName are defined
	// in proto; see the comment on BashPermissionsParams in bash.go.
	GitStatusToolName   = proto.GitStatusToolName
	GitDiffToolName     = proto.GitDiffToolName
	GitLogToolName      = proto.GitLogToolName
	defaultGitOutputCap = 10 << 20
	defaultGitSpoolCap  = 1 << 30
)

var (
	// gitOutputCap is the ceiling on a git command whose whole output is
	// read into memory (runGit). A var, like gitSpoolCap, so a test can
	// lower it: proving that the streaming path is not bound by this cap
	// otherwise means building a fixture bigger than 10MB, which under
	// -race costs more than the rest of the package put together.
	gitOutputCap      = defaultGitOutputCap
	gitSpoolCap       = defaultGitSpoolCap
	gitCommandContext = exec.CommandContext
)

type GitStatusParams struct {
	Paths            []string `json:"paths,omitempty"`
	IncludeUntracked bool     `json:"include_untracked,omitempty"`
	Limit            int      `json:"limit,omitempty"`
	Cursor           string   `json:"cursor,omitempty"`
}
type GitDiffParams struct {
	Mode     string   `json:"mode,omitempty"`
	Base     string   `json:"base,omitempty"`
	Head     string   `json:"head,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Format   string   `json:"format,omitempty"`
	MaxBytes int      `json:"max_bytes,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
}
type GitLogParams struct {
	Revision string   `json:"revision,omitempty"`
	Paths    []string `json:"paths,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
}

type gitStatusEntry struct {
	Path     string `json:"path"`
	OldPath  string `json:"old_path,omitempty"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}
type gitLogEntry struct {
	Hash    string `json:"hash"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}
type gitDiffStatEntry struct {
	Path         string `json:"path"`
	OriginalPath string `json:"original_path,omitempty"`
	Added        string `json:"added"`
	Deleted      string `json:"deleted"`
}
type gitMeta struct {
	Count         int    `json:"count"`
	Total         int    `json:"total"`
	Truncated     bool   `json:"truncated"`
	Cursor        string `json:"cursor,omitempty"`
	Entries       any    `json:"entries,omitempty"`
	TotalBytes    int    `json:"total_bytes,omitempty"`
	RenderedBytes int    `json:"rendered_bytes,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
}

// gitRepo validates the active worktree before every command. Resolving symlinks
// prevents a working directory or path filter from escaping the checked-out tree.
func gitRepo(ctx context.Context, dir string) (string, string, error) {
	wd, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", fmt.Errorf("invalid working directory: %w", err)
	}
	wd, err = filepath.Abs(wd)
	if err != nil {
		return "", "", err
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = wd
	cmd.Env = append(cmd.Environ(), "GIT_EXTERNAL_DIFF=", "GIT_CONFIG_COUNT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("not a git worktree")
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return "", "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	if wd != root && !strings.HasPrefix(wd, root+string(filepath.Separator)) {
		return "", "", fmt.Errorf("working directory is outside git worktree")
	}
	return root, wd, nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	_, wd, err := gitRepo(ctx, dir)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wd
	cmd.Env = append(cmd.Environ(), "GIT_EXTERNAL_DIFF=", "GIT_CONFIG_COUNT=0")
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git: %s", strings.TrimSpace(stderr.String()))
	}
	if out.Len() > gitOutputCap {
		return nil, fmt.Errorf("git output exceeds safety limit")
	}
	return out.Bytes(), nil
}

func safeGitPaths(dir string, in []string) ([]string, error) {
	root, wd, err := gitRepo(context.Background(), dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(in))
	for _, path := range in {
		if path == "" || strings.HasPrefix(path, "-") || filepath.IsAbs(path) {
			return nil, fmt.Errorf("invalid path %q", path)
		}
		joined := filepath.Clean(filepath.Join(wd, path))
		if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			return nil, fmt.Errorf("path outside worktree: %q", path)
		}
		rel, err := filepath.Rel(wd, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path outside working directory: %q", path)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}

func safeRev(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if r < 'a' || r > 'z' {
			if r < 'A' || r > 'Z' {
				if r < '0' || r > '9' {
					if !strings.ContainsRune("._/~^:-", r) {
						return false
					}
				}
			}
		}
	}
	return true
}

func appendPaths(a, p []string) []string {
	if len(p) > 0 {
		a = append(a, "--")
		a = append(a, p...)
	}
	return a
}

func gitResponse(text string, meta gitMeta) fantasy.ToolResponse {
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), meta)
}

func gitError(err error) (fantasy.ToolResponse, error) {
	return fantasy.NewTextErrorResponse(err.Error()), nil
}

func NewGitStatusTool(dir string) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(GitStatusToolName, "Read current git worktree status.", func(ctx context.Context, p GitStatusParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		paths, err := safeGitPaths(dir, p.Paths)
		if err != nil {
			return gitError(err)
		}
		limit := p.Limit
		if limit == 0 {
			limit = 100
		}
		if limit < 1 || limit > 1000 {
			return gitError(fmt.Errorf("limit must be between 1 and 1000"))
		}
		query := fingerprintPage(strings.Join(paths, "\x00"), fmt.Sprint(p.IncludeUntracked))
		cursor, err := openPageKeyCursor(p.Cursor, GitStatusToolName, query)
		if err != nil {
			return gitError(err)
		}
		a := []string{"status", "--porcelain=v1", "-z", "--untracked-files=no"}
		if p.IncludeUntracked {
			a[len(a)-1] = "--untracked-files=all"
		}
		data, err := runGit(ctx, dir, appendPaths(a, paths)...)
		if err != nil {
			return gitError(err)
		}
		entries, err := parseGitStatus(data)
		if err != nil {
			return gitError(err)
		}
		scan := newPageScan[gitStatusEntry](cursor.Last, limit)
		for _, entry := range entries {
			scan.Add(entry.Path+"\x00"+entry.Index+entry.Worktree, entry)
		}
		page, last, truncated, total, gen := scan.Finish()
		if err := finishPageKeyCursor(cursor, gen); err != nil {
			return gitError(err)
		}
		meta := gitMeta{Count: len(page), Total: total, Truncated: truncated, Entries: page}
		if truncated {
			meta.Cursor = makePageKeyCursor(GitStatusToolName, query, gen, last)
		}
		lines := make([]string, len(page))
		for i, e := range page {
			lines[i] = e.Index + e.Worktree + " " + e.Path
			if e.OldPath != "" {
				lines[i] += " (from " + e.OldPath + ")"
			}
		}
		return gitResponse(strings.Join(lines, "\n"), meta), nil
	})
	zero, thousand, one := 0, 1000, 1
	return WithToolSchemaConstraints(tool, map[string]ToolSchemaConstraint{
		"limit": {Minimum: &zero, Maximum: &thousand}, "cursor": {MinLength: &one},
		"paths.items": {MinLength: &one, Pattern: `^[^-].*`},
	})
}

func parseGitStatus(data []byte) ([]gitStatusEntry, error) {
	fields := bytes.Split(data, []byte{0})
	var out []gitStatusEntry
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) == 0 {
			continue
		}
		if len(f) < 4 {
			return nil, fmt.Errorf("invalid git status output")
		}
		e := gitStatusEntry{Index: string(f[0]), Worktree: string(f[1]), Path: string(f[3:])}
		if e.Index == "R" || e.Index == "C" || e.Worktree == "R" || e.Worktree == "C" {
			i++
			if i >= len(fields) {
				return nil, fmt.Errorf("invalid rename status output")
			}
			e.OldPath = string(fields[i])
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Path+out[i].Index+out[i].Worktree < out[j].Path+out[j].Index+out[j].Worktree
	})
	return out, nil
}

func NewGitLogTool(dir string) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(GitLogToolName, "Read git commit history from the current worktree.", func(ctx context.Context, p GitLogParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		paths, err := safeGitPaths(dir, p.Paths)
		if err != nil {
			return gitError(err)
		}
		rev := p.Revision
		if rev == "" {
			rev = "HEAD"
		}
		if !safeRev(rev) {
			return gitError(fmt.Errorf("invalid revision"))
		}
		limit := p.Limit
		if limit == 0 {
			limit = 50
		}
		if limit < 1 || limit > 200 {
			return gitError(fmt.Errorf("limit must be between 1 and 200"))
		}
		query := fingerprintPage(rev, strings.Join(paths, "\x00"))
		cursor, err := openPageKeyCursor(p.Cursor, GitLogToolName, query)
		if err != nil {
			return gitError(err)
		}
		data, err := runGit(ctx, dir, appendPaths([]string{"log", rev, "-z", "--format=%H%x00%an%x00%ae%x00%aI%x00%s"}, paths)...)
		if err != nil {
			if _, headErr := runGit(ctx, dir, "rev-parse", "--verify", "HEAD"); headErr != nil {
				return gitResponse("", gitMeta{Entries: []gitLogEntry{}}), nil
			}
			return gitError(err)
		}
		entries, err := parseGitLog(data)
		if err != nil {
			return gitError(err)
		}
		scan := newPageScan[gitLogEntry](cursor.Last, limit)
		for _, e := range entries {
			scan.Add(e.Date+"\x00"+e.Hash, e)
		}
		page, last, tr, total, gen := scan.Finish()
		if err := finishPageKeyCursor(cursor, gen); err != nil {
			return gitError(err)
		}
		meta := gitMeta{Count: len(page), Total: total, Truncated: tr, Entries: page}
		if tr {
			meta.Cursor = makePageKeyCursor(GitLogToolName, query, gen, last)
		}
		lines := make([]string, len(page))
		for i, e := range page {
			lines[i] = fmt.Sprintf("%s %s %s", e.Hash, e.Date, e.Subject)
		}
		return gitResponse(strings.Join(lines, "\n"), meta), nil
	})
	zero, twoHundred, one := 0, 200, 1
	return WithToolSchemaConstraints(tool, map[string]ToolSchemaConstraint{
		"limit": {Minimum: &zero, Maximum: &twoHundred}, "cursor": {MinLength: &one},
		"revision": {Pattern: `^[A-Za-z0-9._/~^:-]+$`}, "paths.items": {MinLength: &one, Pattern: `^[^-].*`},
	})
}

func parseGitLog(data []byte) ([]gitLogEntry, error) {
	f := bytes.Split(data, []byte{0})
	if len(f) > 0 && len(f[len(f)-1]) == 0 {
		f = f[:len(f)-1]
	}
	if len(f)%5 != 0 {
		return nil, fmt.Errorf("invalid git log output")
	}
	out := make([]gitLogEntry, 0, len(f)/5)
	for i := 0; i < len(f); i += 5 {
		out = append(out, gitLogEntry{string(f[i]), string(f[i+1]), string(f[i+2]), string(f[i+3]), string(f[i+4])})
	}
	return out, nil
}

func NewGitDiffTool(dir string) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(GitDiffToolName, "Read a git diff from the current worktree.", func(ctx context.Context, p GitDiffParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		paths, err := safeGitPaths(dir, p.Paths)
		if err != nil {
			return gitError(err)
		}
		max := p.MaxBytes
		if max == 0 {
			max = 200000
		}
		if max < 1 || max > 200000 {
			return gitError(fmt.Errorf("max_bytes must be between 1 and 200000"))
		}
		mode := p.Mode
		if mode == "" {
			mode = "unstaged"
		}
		a := []string{"-c", "core.quotepath=true", "diff", "--no-ext-diff", "--no-textconv"}
		switch mode {
		case "unstaged":
		case "staged":
			a = append(a, "--cached")
		case "revision":
			if !safeRev(p.Base) || (p.Head != "" && !safeRev(p.Head)) {
				return gitError(fmt.Errorf("invalid revision"))
			}
			r := p.Base
			if p.Head != "" {
				r += ".." + p.Head
			}
			a = append(a, r)
		default:
			return gitError(fmt.Errorf("invalid mode"))
		}
		format := p.Format
		if format == "" {
			format = "patch"
		}
		switch format {
		case "stat":
			a = append(a, "--numstat", "-z")
		case "patch":
			a = append(a, "--patch")
		default:
			return gitError(fmt.Errorf("format must be patch or stat"))
		}
		a = appendPaths(a, paths)
		query := fingerprintPage(mode, p.Base, p.Head, format, strings.Join(paths, "\x00"))
		cursor, err := openPageKeyCursor(p.Cursor, GitDiffToolName, query)
		if err != nil {
			return gitError(err)
		}
		if format == "stat" {
			file, total, gen, err := spoolGitOutput(ctx, dir, false, a...)
			if err != nil {
				return gitError(err)
			}
			defer closeAndRemove(file)
			if err := finishPageKeyCursor(cursor, gen); err != nil {
				return gitError(err)
			}
			page, last, rendered, more, count, err := pageStatFile(file, cursor.Last, max)
			if err != nil {
				return gitError(err)
			}
			text := renderStat(page)
			meta := gitMeta{Count: len(page), Total: count, Truncated: more, Entries: page, TotalBytes: total, RenderedBytes: rendered, SHA256: gen}
			if more {
				meta.Cursor = makePageKeyCursor(GitDiffToolName, query, gen, last)
			}
			return gitResponse(text, meta), nil
		}
		file, total, gen, err := spoolGitOutput(ctx, dir, true, a...)
		if err != nil {
			return gitError(err)
		}
		defer closeAndRemove(file)
		if err := finishPageKeyCursor(cursor, gen); err != nil {
			return gitError(err)
		}
		if cursor.Offset < 0 || cursor.Offset > total {
			return gitError(fmt.Errorf("invalid cursor"))
		}
		page, end, err := readUTF8Page(file, cursor.Offset, total, max)
		if err != nil {
			return gitError(err)
		}
		meta := gitMeta{Count: 1, Total: 1, TotalBytes: total, RenderedBytes: len(page), SHA256: gen, Truncated: end < total}
		if meta.Truncated {
			meta.Cursor, _ = encodePageCursor(pageCursor{Version: 2, Kind: GitDiffToolName, Query: query, Gen: gen, Offset: end})
		}
		return gitResponse(string(page), meta), nil
	})
	zero, maxBytes, one := 0, 200000, 1
	return WithToolSchemaConstraints(tool, map[string]ToolSchemaConstraint{
		"mode": {Enum: []string{"unstaged", "staged", "revision"}}, "format": {Enum: []string{"patch", "stat"}},
		"max_bytes": {Minimum: &zero, Maximum: &maxBytes}, "cursor": {MinLength: &one}, "base": {Pattern: `^[A-Za-z0-9._/~^:-]+$`},
		"head": {Pattern: `^[A-Za-z0-9._/~^:-]+$`}, "paths.items": {MinLength: &one, Pattern: `^[^-].*`},
	})
}

// spoolGitOutput streams raw Git stdout into a temporary file. Patch output is
// normalized to valid UTF-8 so its cursor offsets and digest match returned text.
func spoolGitOutput(ctx context.Context, dir string, normalizeUTF8 bool, args ...string) (*os.File, int, string, error) {
	_, wd, err := gitRepo(ctx, dir)
	if err != nil {
		return nil, 0, "", err
	}
	file, err := os.CreateTemp("", "sennit-git-diff-*")
	if err != nil {
		return nil, 0, "", err
	}
	fail := func(err error) (*os.File, int, string, error) { closeAndRemove(file); return nil, 0, "", err }
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommandContext(child, "git", args...)
	cmd.Dir = wd
	cmd.Env = append(cmd.Environ(), "GIT_EXTERNAL_DIFF=", "GIT_CONFIG_COUNT=0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fail(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	aborted := false
	abort := func() {
		if aborted {
			return
		}
		aborted = true
		cancel()
		_ = stdout.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}
	defer func() {
		if !aborted {
			_ = stdout.Close()
		}
	}()
	hash := sha256.New()
	reader := bufio.NewReader(stdout)
	total := 0
	write := func(chunk []byte) error {
		total += len(chunk)
		if total > gitSpoolCap {
			return fmt.Errorf("git diff exceeds safety limit")
		}
		if _, err := file.Write(chunk); err != nil {
			return err
		}
		_, _ = hash.Write(chunk)
		return nil
	}
	for {
		if normalizeUTF8 {
			r, size, readErr := reader.ReadRune()
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				abort()
				return fail(readErr)
			}
			chunk := []byte(string(r))
			if r == utf8.RuneError && size == 1 {
				chunk = []byte("�")
			}
			if err := write(chunk); err != nil {
				abort()
				return fail(err)
			}
		} else {
			chunk, readErr := reader.ReadBytes(0)
			if len(chunk) > 0 {
				if err := write(chunk); err != nil {
					abort()
					return fail(err)
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				abort()
				return fail(readErr)
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return fail(fmt.Errorf("git: %s", strings.TrimSpace(stderr.String())))
	}
	return file, total, fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func closeAndRemove(file *os.File) {
	if file == nil {
		return
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}

func readUTF8Page(file *os.File, offset, total, max int) ([]byte, int, error) {
	if offset == total {
		return nil, total, nil
	}
	if _, err := file.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, offset, err
	}
	want := min(max+utf8.UTFMax, total-offset)
	b := make([]byte, want)
	n, err := io.ReadFull(file, b)
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, offset, err
	}
	b = b[:n]
	end := min(max, len(b))
	for end > 0 && !utf8.Valid(b[:end]) {
		end--
	}
	if end == 0 {
		return nil, offset, fmt.Errorf("max_bytes is too small for the next UTF-8 character")
	}
	return b[:end], offset + end, nil
}

func statKey(e gitDiffStatEntry) string {
	return e.Path + "\x00" + e.OriginalPath + "\x00" + e.Added + "\x00" + e.Deleted
}

func statLine(e gitDiffStatEntry) string {
	if e.OriginalPath != "" {
		return fmt.Sprintf("%s\t%s\t%s\t(from %s)", e.Added, e.Deleted, e.Path, e.OriginalPath)
	}
	return fmt.Sprintf("%s\t%s\t%s", e.Added, e.Deleted, e.Path)
}

func pageStat(entries []gitDiffStatEntry, after string, budget int) ([]gitDiffStatEntry, string, int, bool, error) {
	page := make([]gitDiffStatEntry, 0)
	rendered := 0
	last := ""
	for _, e := range entries {
		key := statKey(e)
		if key <= after {
			continue
		}
		cost := len(statLine(e))
		if len(page) > 0 {
			cost++
		}
		if cost > budget && len(page) == 0 {
			return nil, "", 0, false, fmt.Errorf("max_bytes is too small for a stat entry")
		}
		if rendered+cost > budget {
			return page, last, rendered, true, nil
		}
		page, last, rendered = append(page, e), key, rendered+cost
	}
	return page, last, rendered, false, nil
}

func pageStatFile(file *os.File, after string, budget int) ([]gitDiffStatEntry, string, int, bool, int, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, false, 0, err
	}
	limit := budget/5 + 2
	h := statEntryHeap{}
	count, eligible := 0, 0
	err := forEachNumstat(file, func(e gitDiffStatEntry) error {
		count++
		if statKey(e) <= after {
			return nil
		}
		eligible++
		if h.Len() < limit {
			heap.Push(&h, e)
			return nil
		}
		if statKey(e) < statKey(h[0]) {
			h[0] = e
			heap.Fix(&h, 0)
		}
		return nil
	})
	if err != nil {
		return nil, "", 0, false, 0, err
	}
	entries := []gitDiffStatEntry(h)
	sort.Slice(entries, func(i, j int) bool { return statKey(entries[i]) < statKey(entries[j]) })
	page, last, rendered, more, err := pageStat(entries, "", budget)
	if err != nil {
		return nil, "", 0, false, 0, err
	}
	return page, last, rendered, more || eligible > len(page), count, nil
}

type statEntryHeap []gitDiffStatEntry

func (h statEntryHeap) Len() int           { return len(h) }
func (h statEntryHeap) Less(i, j int) bool { return statKey(h[i]) > statKey(h[j]) }
func (h statEntryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *statEntryHeap) Push(x any)        { *h = append(*h, x.(gitDiffStatEntry)) }
func (h *statEntryHeap) Pop() any          { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

func forEachNumstat(r io.Reader, visit func(gitDiffStatEntry) error) error {
	br := bufio.NewReader(r)
	for {
		field, err := br.ReadString(0)
		if err == io.EOF && field == "" {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		field = strings.TrimSuffix(field, "\x00")
		parts := strings.SplitN(field, "\t", 3)
		if len(parts) != 3 {
			if err == io.EOF {
				return nil
			}
			continue
		}
		e := gitDiffStatEntry{Added: parts[0], Deleted: parts[1], Path: parts[2]}
		if e.Path == "" {
			old, e1 := br.ReadString(0)
			newPath, e2 := br.ReadString(0)
			if e1 != nil || e2 != nil {
				return fmt.Errorf("invalid git numstat rename output")
			}
			e.OriginalPath, e.Path = strings.TrimSuffix(old, "\x00"), strings.TrimSuffix(newPath, "\x00")
		}
		if err := visit(e); err != nil {
			return err
		}
		if err == io.EOF {
			return nil
		}
	}
}

func renderStat(entries []gitDiffStatEntry) string {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = statLine(e)
	}
	return strings.Join(lines, "\n")
}

package tools

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

// regexCache provides thread-safe caching of compiled regex patterns
type regexCache struct {
	*csync.Map[string, *regexp.Regexp]
}

// newRegexCache creates a new regex cache
func newRegexCache() *regexCache {
	return &regexCache{
		Map: csync.NewMap[string, *regexp.Regexp](),
	}
}

// get retrieves a compiled regex from cache or compiles and caches it
func (rc *regexCache) get(pattern string) (*regexp.Regexp, error) {
	re, ok := rc.Get(pattern)
	if ok && re != nil {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	rc.Set(pattern, re)
	return re, nil
}

// ResetCache clears compiled regex caches to prevent unbounded growth across sessions.
func ResetCache() {
	searchRegexCache.Reset(map[string]*regexp.Regexp{})
	globRegexCache.Reset(map[string]*regexp.Regexp{})
}

// Global regex cache instances
var (
	searchRegexCache = newRegexCache()
	globRegexCache   = newRegexCache()
	// Pre-compiled regex for glob conversion (used frequently)
	globBraceRegex = regexp.MustCompile(`\{([^}]+)\}`)
)

type GrepParams struct {
	Pattern       string `json:"pattern" description:"The regex pattern to search for in file contents"`
	Path          string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include       string `json:"include,omitempty" description:"File pattern to include in the search"`
	LiteralText   bool   `json:"literal_text,omitempty" description:"Treat pattern as literal text"`
	MaxResults    int    `json:"max_results,omitempty" description:"Maximum results (1-1000, defaults to 100)"`
	BeforeContext int    `json:"before_context,omitempty" description:"Lines before each match (0-30)"`
	AfterContext  int    `json:"after_context,omitempty" description:"Lines after each match (0-30)"`
	Cursor        string `json:"cursor,omitempty" description:"Stable continuation token"`
	Sort          string `json:"sort,omitempty" description:"Sort by path or mtime" enum:"path,mtime"`
}

// GrepPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type GrepPermissionsParams = proto.GrepPermissionsParams

type grepMatch struct {
	path     string
	modTime  time.Time
	lineNum  int
	charNum  int
	lineText string
}

type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	TotalMatches    int  `json:"total_matches"`
	Truncated       bool `json:"truncated"`
	// Incomplete is true when part of the search tree could not be walked
	// (a directory removed mid-walk, a permissions denial, EMFILE/ENFILE
	// on a wide tree, ...) and was silently left out of the match set. It
	// is reported separately from Truncated, the same way ls and glob
	// report it: that flag means the result limit cut the matches short,
	// a different fact from part of the tree never having been searched
	// at all.
	Incomplete bool   `json:"incomplete,omitempty"`
	Cursor     string `json:"cursor,omitempty"`
}

const (
	GrepToolName        = "grep"
	maxGrepContentWidth = 500
	// maxGrepContextLines caps before/after context per match. Every extra
	// line is re-read from disk and rendered into the response, so the cap
	// bounds the token cost of a single search.
	maxGrepContextLines = 30
)

//go:embed grep.md.tpl
var grepDescriptionTmpl []byte

var grepDescriptionTpl = template.Must(
	template.New("grepDescription").
		Parse(string(grepDescriptionTmpl)),
)

type grepDescriptionData struct {
	MaxResults int
}

func grepDescription() string {
	return renderTemplate(grepDescriptionTpl, grepDescriptionData{
		MaxResults: 100,
	})
}

// escapeRegexPattern escapes special regex characters so they're treated as literal characters
func escapeRegexPattern(pattern string) string {
	specialChars := []string{"\\", ".", "+", "*", "?", "(", ")", "[", "]", "{", "}", "^", "$", "|"}
	escaped := pattern

	for _, char := range specialChars {
		escaped = strings.ReplaceAll(escaped, char, "\\"+char)
	}

	return escaped
}

func NewGrepTool(permissions permission.Requester, workingDir string, config config.ToolGrep) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(
		GrepToolName,
		grepDescription(),
		func(ctx context.Context, params GrepParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return invalidParam("pattern"), nil
			}
			if params.BeforeContext < 0 || params.BeforeContext > maxGrepContextLines || params.AfterContext < 0 || params.AfterContext > maxGrepContextLines {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("context must be between 0 and %d lines", maxGrepContextLines)), nil
			}
			limit := params.MaxResults
			if limit == 0 {
				limit = 100
			}
			if limit < 1 || limit > maxPageResults {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("max_results must be between 1 and %d", maxPageResults)), nil
			}
			if params.Sort != "" && params.Sort != "path" && params.Sort != "mtime" {
				return fantasy.NewTextErrorResponse("sort must be path or mtime"), nil
			}
			if params.Sort == "" {
				params.Sort = "mtime"
			}
			pattern := params.Pattern
			if params.LiteralText {
				pattern = escapeRegexPattern(pattern)
			}
			searchPath := filepathext.SmartJoin(workingDir, params.Path)

			absSearchPath, resolvedSearchPath, outside, err := resolveWithinWorkdir(workingDir, searchPath)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("resolve path: %w", err)
			}
			if outside {
				resp, denied, err := requireOutsideWorkdirPermission(
					ctx, permissions, call,
					GrepToolName, "search", "Search file contents outside working directory",
					"searching file contents outside working directory",
					absSearchPath, resolvedSearchPath, GrepPermissionsParams(params),
				)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return resp, nil
				}
			}

			searchCtx, cancel := context.WithTimeout(ctx, config.GetTimeout())
			defer cancel()
			query := fingerprintPage(canonicalPath(searchPath), params.Pattern, params.Include, fmt.Sprint(params.LiteralText), params.Sort, fmt.Sprint(params.BeforeContext), fmt.Sprint(params.AfterContext))
			continuation, err := openPageKeyCursor(params.Cursor, "grep", query)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			scan := newPageScan[grepMatch](continuation.Last, limit)
			incomplete, err := visitSearchMatches(searchCtx, pattern, searchPath, params.Include, func(match grepMatch) {
				scan.Add(grepMatchPageKey(match, params.Sort), match)
			})
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error searching files: %v", err)), nil
			}
			page, last, truncated, total, generation := scan.Finish()
			if err := finishPageKeyCursor(continuation, generation); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			cursor := ""
			if truncated {
				cursor = makePageKeyCursor("grep", query, generation, last)
			}
			output, err := renderGrepMatchesWithContext(searchCtx, page, truncated, params.BeforeContext, params.AfterContext)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("rendering search context: %w", err)
			}
			if incomplete {
				// Mirrors ls/glob: part of the tree could not be read
				// (removed mid-walk, a permissions denial, EMFILE/ENFILE
				// on a wide tree, ...), so matches may be missing for a
				// reason the cursor cannot fix by paging further.
				output += "\n\n(Part of the search tree could not be read, so some matches may be missing. Retry or narrow the path to confirm.)"
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), GrepResponseMetadata{NumberOfMatches: len(page), TotalMatches: total, Truncated: truncated, Incomplete: incomplete, Cursor: cursor}), nil
		},
	)
	return withToolParameterSchema(tool, map[string]toolParameterSchema{
		"pattern":        {minLength: intPtr(1)},
		"max_results":    intSchemaBounds(maxPageResults),
		"before_context": intSchemaBounds(maxGrepContextLines),
		"after_context":  intSchemaBounds(maxGrepContextLines),
	})
}

func searchFiles(ctx context.Context, pattern, rootPath, include string, limit int) ([]grepMatch, bool, error) {
	matches, err := searchFilesWithRegex(ctx, pattern, rootPath, include)
	if err != nil {
		return nil, false, err
	}
	matches, truncated := sortAndTruncateMatches(matches, limit)
	return matches, truncated, nil
}

// sortAndTruncateMatches orders matches by file modification time (newest
// first) and caps them at limit. It uses a stable sort so that the multiple
// matches a single file can contribute (all sharing the same modTime) keep
// their original line order and stay grouped together in the rendered
// output.
func grepMatchKey(m grepMatch) string {
	return fmt.Sprintf("%s\x00%020d\x00%020d\x00%s", filepath.ToSlash(m.path), m.lineNum, m.charNum, fingerprintPage(m.lineText))
}

func grepMatchPageKey(m grepMatch, order string) string {
	if order == "mtime" {
		// Invert the timestamp so the lexicographic key orders newest first.
		return fmt.Sprintf("%020d\x00%s", ^uint64(m.modTime.UnixNano()), grepMatchKey(m))
	}
	return grepMatchKey(m)
}

func sortAndTruncateMatches(matches []grepMatch, limit int) ([]grepMatch, bool) {
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].modTime.After(matches[j].modTime)
	})

	truncated := len(matches) > limit
	if truncated {
		matches = matches[:limit]
	}

	return matches, truncated
}

func renderGrepMatchesWithContext(ctx context.Context, matches []grepMatch, truncated bool, before, after int) (string, error) {
	contexts, err := loadGrepContexts(ctx, matches, before, after, os.Open)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	if len(matches) == 0 {
		output.WriteString("No files found")
		return output.String(), nil
	}

	fmt.Fprintf(&output, "Found %d matches\n", len(matches))

	currentFile := ""
	for _, match := range matches {
		if currentFile != match.path {
			if currentFile != "" {
				output.WriteString("\n")
			}
			currentFile = match.path
			fmt.Fprintf(&output, "%s:\n", filepath.ToSlash(match.path))
		}
		if match.lineNum > 0 {
			for _, line := range contexts[match.path][match.lineNum] {
				fmt.Fprintf(&output, "  %s Line %d: %s\n", line.marker, line.number, truncateGrepLine(line.text))
			}
			lineText := match.lineText
			lineText = truncateGrepLine(lineText)
			if match.charNum > 0 {
				fmt.Fprintf(&output, "  Line %d, Char %d: %s\n", match.lineNum, match.charNum, lineText)
			} else {
				fmt.Fprintf(&output, "  Line %d: %s\n", match.lineNum, lineText)
			}
		} else {
			fmt.Fprintf(&output, "  %s\n", match.path)
		}
	}

	if truncated {
		output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
	}

	return output.String(), nil
}

type grepContextLine struct {
	number       int
	text, marker string
}

func truncateGrepLine(line string) string {
	if ansi.StringWidth(line) > maxGrepContentWidth {
		return ansi.Truncate(line, maxGrepContentWidth, "...")
	}
	return line
}

type grepContextOpener func(string) (*os.File, error)

func loadGrepContexts(ctx context.Context, matches []grepMatch, before, after int, open grepContextOpener) (map[string]map[int][]grepContextLine, error) {
	result := make(map[string]map[int][]grepContextLine)
	if before == 0 && after == 0 {
		return result, nil
	}
	wanted := make(map[string]map[int]struct{})
	maxLine := make(map[string]int)
	for _, match := range matches {
		lines := wanted[match.path]
		if lines == nil {
			lines = make(map[int]struct{})
			wanted[match.path] = lines
		}
		for line := max(1, match.lineNum-before); line <= match.lineNum+after; line++ {
			if line != match.lineNum {
				lines[line] = struct{}{}
			}
		}
		maxLine[match.path] = max(maxLine[match.path], match.lineNum+after)
	}
	for path, lines := range wanted {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, err := open(path)
		if err != nil {
			continue
		}
		text := make(map[int]string, len(lines))
		reader := bufio.NewReader(file)
		for lineNum := 1; lineNum <= maxLine[path]; lineNum++ {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return nil, err
			}
			line, readErr := reader.ReadString('\n')
			if _, ok := lines[lineNum]; ok {
				text[lineNum] = strings.TrimRight(line, "\r\n")
			}
			if readErr != nil {
				break
			}
		}
		_ = file.Close()
		perMatch := make(map[int][]grepContextLine)
		for _, match := range matches {
			if match.path != path {
				continue
			}
			for line := max(1, match.lineNum-before); line <= match.lineNum+after; line++ {
				value, ok := text[line]
				if !ok || line == match.lineNum {
					continue
				}
				marker := "-"
				if line > match.lineNum {
					marker = "+"
				}
				perMatch[match.lineNum] = append(perMatch[match.lineNum], grepContextLine{line, value, marker})
			}
		}
		result[path] = perMatch
	}
	return result, nil
}

func searchFilesWithRegex(ctx context.Context, pattern, rootPath, include string) ([]grepMatch, error) {
	matches := []grepMatch{}
	_, err := visitSearchMatches(ctx, pattern, rootPath, include, func(match grepMatch) {
		matches = append(matches, match)
	})
	return matches, err
}

// visitSearchMatches walks rootPath and calls visit for every matching
// line. incomplete reports whether any part of the tree could not be
// walked (a directory removed mid-walk, a permissions denial, EMFILE/ENFILE
// on a wide tree, ...) — such a subtree is simply absent from what visit
// was called with, so the caller must surface incomplete itself or a
// partial match set reads as an exhaustive one.
func visitSearchMatches(ctx context.Context, pattern, rootPath, include string, visit func(grepMatch)) (incomplete bool, err error) {
	// Use cached regex compilation
	regex, err := searchRegexCache.get(pattern)
	if err != nil {
		return false, fmt.Errorf("invalid regex pattern: %w", err)
	}

	var includePattern *regexp.Regexp
	if include != "" {
		// Anchored, and to a path segment: the translated glob is matched
		// against the whole path with MatchString, which is a substring
		// test. Unanchored, "*.js" matched "app.json" (it contains ".js")
		// and "*.c" matched every .cpp and .css in the tree, so an
		// include narrowed the search to rather more than it named.
		regexPattern := "(^|/)" + globToRegex(include) + "$"
		includePattern, err = globRegexCache.get(regexPattern)
		if err != nil {
			return false, fmt.Errorf("invalid include pattern: %w", err)
		}
	}

	// Create walker with gitignore and sennitignore support, keyed on
	// rootPath as given — see the walkRoot/virtualPath translation below,
	// which keeps every path handed to it in that same namespace.
	walker := fsext.NewFastGlobWalker(rootPath)

	// filepath.Walk lstats its root, so a directory-symlink root (e.g. a
	// "current" symlink into a dated release directory) reports
	// IsDir()==false and the walk stops after that single, dropped entry
	// — silently returning zero matches instead of searching the target.
	// Resolve it once up front and walk the target instead, translating
	// every visited path back into rootPath's own namespace below: the
	// caller asked to search "current/", and matches must be reported
	// under that name, not the release directory it happens to resolve
	// to. This is the same fix fsext.VisitDirectory applies for ls (see
	// fsext/ls.go) — glob does not need it because fastwalk stats
	// (rather than lstats) its root.
	walkRoot := rootPath
	if lst, lerr := os.Lstat(rootPath); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
		if target, terr := filepath.EvalSymlinks(rootPath); terr == nil {
			if tinfo, serr := os.Stat(target); serr == nil && tinfo.IsDir() {
				walkRoot = target
			}
		}
	}

	err = filepath.Walk(walkRoot, func(path string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// A subtree filepath.Walk could not read (removed mid-walk,
			// a permissions denial, EMFILE/ENFILE on a wide tree, ...):
			// it is dropped from the results, so the caller must be told
			// the match set is not exhaustive rather than reading a
			// silently partial tree as a complete, merely small one.
			incomplete = true
			return nil
		}

		rel, relErr := filepath.Rel(walkRoot, path)
		if relErr != nil {
			return nil
		}
		virtualPath := rootPath
		if rel != "." {
			virtualPath = filepath.Join(rootPath, rel)
		}

		if info.IsDir() {
			// Check if directory should be skipped
			if walker.ShouldSkip(virtualPath) {
				return filepath.SkipDir
			}
			return nil // Continue into directory
		}

		// Use walker's shouldSkip method for files
		if walker.ShouldSkip(virtualPath) {
			return nil
		}

		// Skip hidden files (starting with a dot) to match ripgrep's default behavior
		base := filepath.Base(virtualPath)
		if base != "." && strings.HasPrefix(base, ".") {
			return nil
		}

		if includePattern != nil && !includePattern.MatchString(virtualPath) {
			return nil
		}

		matchErr := visitFileMatches(ctx, virtualPath, regex, func(lm lineMatch) {
			visit(grepMatch{
				path:     virtualPath,
				modTime:  info.ModTime(),
				lineNum:  lm.lineNum,
				charNum:  lm.charNum,
				lineText: lm.lineText,
			})
		})
		if matchErr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	return incomplete, err
}

// lineMatch is a single matching line within a file: its 1-based line
// number, the 1-based column of the first match on that line, and the
// line text (with the trailing newline stripped).
type lineMatch struct {
	lineNum  int
	charNum  int
	lineText string
}

func visitFileMatches(ctx context.Context, filePath string, pattern *regexp.Regexp, visit func(lineMatch)) error {
	if pattern == nil || !isTextFile(filePath) {
		return nil
	}
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	lineNum := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		lineNum++
		line = strings.TrimSuffix(line, "\n")
		line = strings.TrimSuffix(line, "\r")
		if loc := pattern.FindStringIndex(line); loc != nil {
			visit(lineMatch{lineNum: lineNum, charNum: loc[0] + 1, lineText: line})
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// isTextFile checks if a file is a text file by examining its MIME type.
func isTextFile(filePath string) bool {
	file, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer file.Close()

	// Read first 512 bytes for MIME type detection.
	buffer := make([]byte, 512)
	n, err := file.Read(buffer)
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}

	// Detect content type.
	contentType := http.DetectContentType(buffer[:n])

	// Check if it's a text MIME type.
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/javascript" ||
		contentType == "application/x-sh"
}

func globToRegex(glob string) string {
	regexPattern := strings.ReplaceAll(glob, ".", "\\.")
	regexPattern = strings.ReplaceAll(regexPattern, "*", ".*")
	regexPattern = strings.ReplaceAll(regexPattern, "?", ".")

	// Use pre-compiled regex instead of compiling each time
	regexPattern = globBraceRegex.ReplaceAllStringFunc(regexPattern, func(match string) string {
		inner := match[1 : len(match)-1]
		return "(" + strings.ReplaceAll(inner, ",", "|") + ")"
	})

	return regexPattern
}

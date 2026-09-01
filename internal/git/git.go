// Package git shells out to the git CLI for the repository operations
// needed by threads (parallel agent work streams, each running in its own
// git worktree and auto-merged back into a base branch). It deliberately
// avoids go-git for repo operations — go-git in this codebase is used only
// for .gitignore parsing — so behavior matches whatever git binary the user
// has on PATH.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/rave-soft/sennit/internal/fsext"
)

// Sentinel errors returned by [FastForward]. Callers use errors.Is to
// distinguish "the ref moved on and needs a real merge" from "someone has
// the branch checked out and it can't be fast-forwarded from outside".
var (
	// ErrNonFastForward is returned when the target ref is not an ancestor
	// of the source branch, so a fast-forward update was refused.
	ErrNonFastForward = errors.New("git: update is not a fast-forward")
	// ErrBranchCheckedOut is returned when git refuses to update a branch
	// because it is checked out in some worktree.
	ErrBranchCheckedOut = errors.New("git: branch is checked out in another worktree")
	// ErrNotRepository means git definitively reported that the target is not
	// in a repository. It deliberately excludes command, context, permission,
	// and other operational failures so callers can fail closed.
	ErrNotRepository = errors.New("git: not a repository")
)

// commandContext is the git command construction seam. Tests replace it to
// verify operational failures without changing PATH or invoking a shell.
var commandContext = exec.CommandContext

// run executes git with the given args in dir and returns trimmed stdout.
// On failure the returned error wraps git's stderr so callers get an
// actionable message without having to inspect *exec.ExitError themselves.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

func runRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := commandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Force a C locale so error-message matching (see FastForward) doesn't
	// depend on the user's system locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if isNotRepository(err, stderr.String()) {
			return "", fmt.Errorf("%w: git %s: %w: %s", ErrNotRepository, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IsRepo reports whether dir is inside a git working tree (or repo dir).
// isNotRepository recognizes git's stable fatal diagnostic for rev-parse.
// Git emits this only after successfully running and examining dir; all other
// errors remain operational failures. The C locale set by runRaw keeps this
// classification independent of the user's locale.
func isNotRepository(err error, stderr string) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && strings.Contains(stderr, "not a git repository")
}

func IsRepo(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// CommonDir returns the canonical absolute path to the git directory shared by
// every worktree in the repository containing dir.
func CommonDir(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git: common dir: %w", err)
	}
	return canonicalCommonDir(dir, out)
}

// canonicalCommonDir resolves rev-parse's relative --git-common-dir output
// relative to the working directory supplied to git, then canonicalizes it.
func canonicalCommonDir(workingDir, commonDir string) (string, error) {
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workingDir, commonDir)
	}
	commonDir, err := filepath.Abs(filepath.Clean(commonDir))
	if err != nil {
		return "", fmt.Errorf("git: absolute common dir: %w", err)
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", fmt.Errorf("git: canonical common dir: %w", err)
	}
	return filepath.Clean(commonDir), nil
}

// TopLevel returns the absolute path to the top-level directory of the
// repository containing dir.
func TopLevel(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git: top level: %w", err)
	}
	return out, nil
}

// CurrentBranch returns the name of the branch checked out in dir. If HEAD
// is detached, git prints "HEAD" and CurrentBranch returns that literal
// string with no error — callers that care about detached HEAD should
// check for it explicitly.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	out, err := run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git: current branch: %w", err)
	}
	return out, nil
}

// IsDirty reports whether dir has uncommitted changes (staged, unstaged,
// or untracked).
func IsDirty(ctx context.Context, dir string) (bool, error) {
	out, err := run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git: status: %w", err)
	}
	return out != "", nil
}

// FileChange describes an uncommitted file and its line statistics.
type FileChange struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// ErrNotARepo reports that the directory asked about is not inside a Git
// working tree.
//
// It matters that this is an error rather than an empty result: "git says
// nothing about these files" and "git says every one of them is committed"
// are the same empty list, and callers that decide whether a file still
// has pending changes need to tell the two apart. See
// workspace.PrepareSessionChangesUsing.
var ErrNotARepo = errors.New("git: not a repository")

// UncommittedFiles returns staged, unstaged, and untracked files in dir's
// repository. A directory outside a Git repository returns ErrNotARepo.
func UncommittedFiles(ctx context.Context, dir string) ([]FileChange, error) {
	if !IsRepo(ctx, dir) {
		return nil, ErrNotARepo
	}

	repo, err := TopLevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	status, err := runRaw(ctx, repo, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("git: uncommitted files: %w", err)
	}
	if status == "" {
		return []FileChange{}, nil
	}

	changes := make(map[string]FileChange)
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if len(entry) < 4 {
			continue
		}
		path := entry[3:]
		changes[path] = FileChange{Path: filepath.Join(repo, path)}
		if entry[0] == 'R' || entry[0] == 'C' || entry[1] == 'R' || entry[1] == 'C' {
			i++ // Porcelain -z includes the original path as the next field.
		}
	}

	// A brand-new repo has no commits yet, so HEAD doesn't resolve to
	// anything and `git diff HEAD` fails outright. Skip the numstat step in
	// that case: every file is new, so the by-hand line count below (which
	// already fires for anything with no diff stats) covers it.
	if _, err := run(ctx, repo, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		// -z: without it, a quoted path (non-ASCII or containing a special
		// character) never matches the status map's key, silently falling
		// back to the by-hand line count below for that file. -z also
		// changes the rename record shape: "add\tdel\t" (no third field) is
		// followed by two more NUL-terminated tokens, the old and new path.
		numstat, err := runRaw(ctx, repo, "diff", "--numstat", "-z", "HEAD", "--")
		if err != nil {
			return nil, fmt.Errorf("git: diff stats: %w", err)
		}
		tokens := strings.Split(numstat, "\x00")
		for i := 0; i < len(tokens); i++ {
			tok := tokens[i]
			if tok == "" {
				continue
			}
			parts := strings.SplitN(tok, "\t", 3)
			if len(parts) < 2 {
				continue
			}
			path := ""
			if len(parts) == 3 {
				path = parts[2]
			} else if i+2 < len(tokens) {
				// Rename/copy: this token only carries add/del counts; the
				// next two NUL-separated tokens are the old and new path.
				// The map is keyed by the new path, matching how status -z
				// records a rename above.
				i += 2
				path = tokens[i]
			}
			change, ok := changes[path]
			if !ok {
				continue
			}
			change.Additions, _ = strconv.Atoi(parts[0])
			change.Deletions, _ = strconv.Atoi(parts[1])
			changes[path] = change
		}
	}

	result := make([]FileChange, 0, len(changes))
	for path, change := range changes {
		if change.Additions == 0 && change.Deletions == 0 {
			content, readErr := os.ReadFile(filepath.Join(repo, path))
			if readErr == nil && !bytes.Contains(content, []byte{0}) {
				change.Additions = bytes.Count(content, []byte{'\n'})
				if len(content) > 0 && content[len(content)-1] != '\n' {
					change.Additions++
				}
			}
		}
		result = append(result, change)
	}
	slices.SortFunc(result, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })
	return result, nil
}

// BranchExists reports whether a local branch named name exists in repo.
func BranchExists(ctx context.Context, repo, name string) (bool, error) {
	_, err := run(ctx, repo, "rev-parse", "--verify", "refs/heads/"+name)
	if err != nil {
		// git rev-parse --verify fails both for a missing ref and for
		// other errors, but for our purposes a failed verify means the
		// branch does not exist.
		return false, nil
	}
	return true, nil
}

// WorktreeAdd creates a new worktree at path, checking out a new branch
// newBranch created from base.
//
// This is two git calls rather than the single "worktree add -b newBranch
// path base" one might expect. "worktree add" re-derives its branch-creation
// logic from newBranch without ever treating it as an end-of-options
// boundary would: "worktree add -b -f path base" parses "-f" as --force and
// tries to force-update whatever is checked out at base, and "-d" tries to
// delete it, instead of failing on the branch name (verified against git
// 2.53). "git branch -- newBranch base" does not have that problem — with
// "--" in place it treats newBranch strictly as a name and rejects a
// leading dash the same way it always rejects an invalid branch name, so
// it fails closed instead of doing something unrelated to the caller's
// request. The second call then just checks out the branch that already
// exists.
//
// Two calls means a window the single-command form never had: if the first
// succeeds and the second fails (path already occupied, a stale worktree
// registration, the branch already checked out elsewhere, ...), newBranch
// would otherwise be left behind even though no worktree exists for it. The
// only production caller (thread creation) reuses the same branch name on
// retry, so a leftover branch there would turn one transient failure into a
// permanently stuck name via "branch already exists". Delete newBranch back
// out on that path so failure here leaves nothing behind, matching what the
// atomic single-command form would have left. The cleanup is best-effort:
// its own failure is reported alongside the original error rather than
// replacing it, since the original is what the caller actually needs to
// see.
func WorktreeAdd(ctx context.Context, repo, path, newBranch, base string) error {
	if _, err := run(ctx, repo, "branch", "--", newBranch, base); err != nil {
		return fmt.Errorf("git: worktree add: %w", err)
	}
	if _, err := run(ctx, repo, "worktree", "add", "--", path, newBranch); err != nil {
		if delErr := DeleteBranch(ctx, repo, newBranch, true); delErr != nil {
			return fmt.Errorf("git: worktree add: %w (cleanup of branch %q also failed: %w)", err, newBranch, delErr)
		}
		return fmt.Errorf("git: worktree add: %w", err)
	}
	return nil
}

// WorktreeRemove removes the worktree at path and deregisters it from
// repo's worktree metadata. If force is true, uncommitted changes and
// untracked files in the worktree are discarded without complaint.
//
// Removing something that is already gone succeeds: a caller cleaning up
// after a worktree someone deleted by hand wants the registration pruned
// and its own record dropped, not an error it can never get past.
//
// A forced remove deletes the working files itself rather than letting git
// do it, after restoring write permission on the worktree's directories.
// Two reasons:
//
// Anything a thread builds can leave directories behind that git cannot
// unlink through — Go's module cache marks every package directory 0555,
// and a thread whose GOMODCACHE points inside its own worktree fills it
// with thousands of them — so the permissions have to be repaired first.
//
// And some of them cannot be repaired at all: a container that wrote into
// the worktree owns its directories, and no chmod by this process will
// make them writable. git gives us exactly one attempt at those, because
// it deletes the worktree's administrative directory under .git/worktrees
// *before* it deletes the working files: a run that dies partway leaves a
// full worktree on disk that git has already forgotten, no later git
// command will touch it, a retry reports only "is not a working tree",
// and the directory leaks permanently. Deleting the files ourselves keeps
// the registration intact until they are actually gone, so a failure is
// reported against a worktree that still exists and can be retried once
// whatever owns those directories has released them.
func WorktreeRemove(ctx context.Context, repo, path string, force bool) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return deregisterMissingWorktree(ctx, repo, path)
	}
	// Only a forced remove is entitled to delete the directory itself, and
	// only once the path has been positively identified as one of this
	// repo's linked worktrees.
	if force && ownedLinkedWorktree(ctx, repo, path) {
		makeDirsWritable(path)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("git: worktree remove: %s: %w", path, err)
		}
		return deregisterMissingWorktree(ctx, repo, path)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	// "--" keeps a path beginning with "-" from being parsed as an option.
	args = append(args, "--", path)
	if _, err := run(ctx, repo, args...); err != nil {
		return fmt.Errorf("git: worktree remove: %w", err)
	}
	// git's own "worktree remove" above already deregistered this one
	// entry on success — no separate cleanup step is needed.
	return nil
}

// deregisterMissingWorktree drops repo's registration for the worktree at
// path once its files are confirmed gone from disk (path does not exist).
//
// This used to be a bare "git worktree prune", but prune has no flag to
// scope itself to one worktree (checked: only -n/--dry-run, -v, and
// --expire exist). Repo-wide, it deregisters every prunable worktree at
// once — including one left in exactly the hazardous state a partially
// failed removal elsewhere produces: its own .git pointer file deleted by
// an interrupted os.RemoveAll (which removes entries in sorted order, and
// ".git" sorts first) while its other files are still on disk. Once that
// entry is swept up, ownedLinkedWorktree can no longer identify the path as
// this repo's own, so Sennit refuses to touch it and the files become
// unreclaimable — precisely what pruning after a removal was meant to
// prevent.
//
// "git worktree remove <path>" targets exactly one registry entry and
// succeeds even when path no longer exists on disk (verified: it removes a
// registered-but-vanished worktree without --force), so it replaces prune
// here without narrowing the window — nothing else in the repo is touched.
// The registration is checked first so a path that was never registered,
// or was already deregistered by an earlier run, is still a silent
// success, matching what bare prune used to do for it; main and locked
// worktrees are left alone for the same reason prune itself skips them.
func deregisterMissingWorktree(ctx context.Context, repo, path string) error {
	reg, err := worktreeRegistration(ctx, repo, path)
	if err != nil {
		return err
	}
	if !reg.registered || reg.main || reg.locked {
		return nil
	}
	if _, err := run(ctx, repo, "worktree", "remove", "--", path); err != nil {
		return fmt.Errorf("git: worktree remove: %w", err)
	}
	return nil
}

// makeDirsWritable adds owner write permission to every directory at or
// below root, so that their entries can be unlinked. Only directories are
// touched: unlinking a file needs write permission on its parent, not on
// the file, so a read-only file is no obstacle and is left as it is.
//
// Symlinks are never followed — [filepath.WalkDir] reports them as
// non-directory entries — so this cannot reach outside root.
func makeDirsWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			// A directory that cannot be read is one this walk cannot
			// fix; leave the removal itself to report what it means.
			return nil //nolint:nilerr // best effort, see the doc comment
		}
		info, err := d.Info()
		if err != nil || info.Mode().Perm()&0o200 != 0 {
			return nil
		}
		// 0o300 rather than 0o200: a directory also needs the execute
		// bit to be traversed, and a mode like 0o444 has neither.
		_ = os.Chmod(path, info.Mode().Perm()|0o300)
		return nil
	})
}

// ownedLinkedWorktree reports whether path is a linked worktree of repo
// that [WorktreeRemove] may delete outright. The guard is deliberately
// narrow, because the outcome is an unrecoverable delete of whatever is at
// path: it must be positively identified, and everything unrecognized is
// left to git and its error message.
//
// Two shapes qualify. Normally git still has the worktree registered, which
// is what an interrupted removal leaves behind too: a partial delete takes
// the worktree's own .git pointer with it, so only the registry can still
// name what a retry is looking at. And a worktree an older build managed to
// deregister before deleting is identified from that pointer instead — git
// has forgotten it, so nothing else will ever clean it up.
//
// A locked worktree is not ours to delete: nothing here locks one, so
// somebody wanted it kept, and git's refusal is the right answer. Neither
// is the main worktree — it is registered like any other, but deleting it
// would take the repository with it.
func ownedLinkedWorktree(ctx context.Context, repo, path string) bool {
	switch reg, err := worktreeRegistration(ctx, repo, path); {
	case err != nil:
		return false
	case reg.registered:
		return !reg.main && !reg.locked
	}
	// Not registered: the only remaining candidate is a worktree of this
	// repo whose administrative directory is already gone.
	adminDir, ok := linkedWorktreeAdminDir(ctx, repo, path)
	if !ok {
		return false
	}
	_, err := os.Lstat(adminDir)
	return errors.Is(err, fs.ErrNotExist)
}

// worktreeReg is what repo's worktree registry says about one path.
type worktreeReg struct {
	registered bool
	main       bool
	locked     bool
}

// worktreeRegistration reports how repo currently registers the worktree at
// path, as read from "git worktree list --porcelain". Records are separated
// by blank lines and open with a "worktree <path>" line; git lists the main
// worktree first and marks a locked one with a "locked" line.
func worktreeRegistration(ctx context.Context, repo, path string) (worktreeReg, error) {
	var reg worktreeReg
	out, err := run(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		return reg, fmt.Errorf("git: worktree list: %w", err)
	}
	target := canonicalPath(path)
	record, match := -1, false
	for line := range strings.SplitSeq(out, "\n") {
		if listed, ok := strings.CutPrefix(line, "worktree "); ok {
			record++
			match = canonicalPath(listed) == target
			if match {
				reg.registered = true
				reg.main = record == 0
			}
			continue
		}
		if match && (line == "locked" || strings.HasPrefix(line, "locked ")) {
			reg.locked = true
		}
	}
	return reg, nil
}

// canonicalPath resolves path as far as it can for comparison against the
// paths git reports, which are absolute and symlink-free. A path that
// cannot be resolved — one that no longer exists, say — is still cleaned,
// so two spellings of the same missing path compare equal, including when
// the missing path sits under an aliased ancestor (a symlinked parent
// directory, or a Windows short name) — see [fsext.Canonical], which does
// the actual work and is shared with the fsext package's own path
// comparisons rather than kept as a near-copy here.
func canonicalPath(path string) string {
	return fsext.Canonical(path)
}

// linkedWorktreeAdminDir returns the administrative directory registering
// the linked worktree at path with repo — its .git/worktrees/<name> entry —
// and whether path looks like one of repo's linked worktrees at all.
//
// Both of the following must hold: path holds a .git file (not a directory
// — that would be a repository in its own right, never a linked worktree),
// and that file names a gitdir under this repo's own worktrees directory,
// which another repository's worktree does not. The directory it names need
// not still exist; see [ownedLinkedWorktree], the only caller.
func linkedWorktreeAdminDir(ctx context.Context, repo, path string) (string, bool) {
	gitFile, err := os.Lstat(filepath.Join(path, ".git"))
	if err != nil || !gitFile.Mode().IsRegular() {
		return "", false
	}
	content, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return "", false
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir: ")
	if !ok {
		return "", false
	}
	// The common dir, not --git-dir: called from inside a worktree the
	// latter points at that worktree's own administrative directory,
	// while worktrees are always registered under the shared one.
	commonDir, err := run(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", false
	}
	worktrees := filepath.Join(commonDir, "worktrees") + string(filepath.Separator)
	gitDir = filepath.Clean(gitDir)
	if !strings.HasPrefix(gitDir+string(filepath.Separator), worktrees) {
		return "", false
	}
	return gitDir, true
}

// DeleteBranch deletes the local branch name. If force is true, the branch
// is deleted even if it is not fully merged (-D instead of -d).
//
// A branch that is already gone is not an error, for the same reason a
// worktree that is already gone is not one in [WorktreeRemove]: a caller
// finishing a cleanup someone started by hand has nothing left to do, and
// failing here would leave it stuck on a branch nobody can delete twice.
func DeleteBranch(ctx context.Context, repo, name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	// "--" keeps a branch name beginning with "-" from being parsed as a
	// second flag.
	if _, err := run(ctx, repo, "branch", flag, "--", name); err != nil {
		if exists, existsErr := BranchExists(ctx, repo, name); existsErr == nil && !exists {
			return nil
		}
		return fmt.Errorf("git: delete branch: %w", err)
	}
	return nil
}

// IsAncestor reports whether ref is reachable from other — i.e. whether
// everything on ref is already contained in other.
//
// This is the check "git branch -d" only appears to make: that one asks
// whether the branch is merged into HEAD (or its upstream), so it refuses
// a branch that is fully merged into some other branch while an unrelated
// one happens to be checked out. Ask about the two branches that actually
// matter instead.
func IsAncestor(ctx context.Context, repo, ref, other string) (bool, error) {
	// "--" keeps a ref beginning with "-" from being parsed as an option.
	_, err := run(ctx, repo, "merge-base", "--is-ancestor", "--", ref, other)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	// Exit code 1 is the answer "no", not a failure to answer.
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git: is-ancestor: %w", err)
}

// CommitAll stages all changes in worktree (git add -A) and commits them
// with message. If there is nothing to commit, it returns (false, nil)
// rather than treating an empty commit as an error.
func CommitAll(ctx context.Context, worktree, message string) (bool, error) {
	if _, err := run(ctx, worktree, "add", "-A"); err != nil {
		return false, fmt.Errorf("git: add: %w", err)
	}

	dirty, err := IsDirty(ctx, worktree)
	if err != nil {
		return false, err
	}
	if !dirty {
		return false, nil
	}

	if _, err := run(ctx, worktree, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("git: commit: %w", err)
	}
	return true, nil
}

// MergeResult describes the outcome of [MergeIntoWorktree].
type MergeResult struct {
	// Merged is true when the merge completed (including no-op merges of
	// an already-up-to-date ref).
	Merged bool
	// Conflicts lists the paths left in conflicted state when Merged is
	// false. Empty when Merged is true.
	Conflicts []string
}

// MergeIntoWorktree merges ref into whatever branch is checked out in
// worktree, using --no-edit so git never blocks on an editor. On a clean
// merge it returns MergeResult{Merged: true}. On conflicts it returns
// MergeResult{Merged: false, Conflicts: [...]} with a nil error — a merge
// conflict is an expected outcome for callers to handle, not a failure of
// the git invocation itself. Any other git failure is returned as an error.
func MergeIntoWorktree(ctx context.Context, worktree, ref string) (*MergeResult, error) {
	// "--" keeps a ref beginning with "-" from being parsed as an option.
	_, mergeErr := run(ctx, worktree, "merge", "--no-edit", "--", ref)
	if mergeErr == nil {
		return &MergeResult{Merged: true}, nil
	}

	conflicts, listErr := run(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
	if listErr != nil {
		// The merge failed for a reason unrelated to conflicts (bad ref,
		// dirty worktree refusing to merge, etc.) — surface the original
		// merge error.
		return nil, fmt.Errorf("git: merge: %w", mergeErr)
	}
	if conflicts == "" {
		// Merge failed but left no conflicted paths — not a conflict we
		// can report, so surface the original error.
		return nil, fmt.Errorf("git: merge: %w", mergeErr)
	}

	return &MergeResult{Merged: false, Conflicts: strings.Split(conflicts, "\n")}, nil
}

// ConflictedFiles lists the paths currently left in conflicted (unmerged)
// state in worktree. Empty when there is no merge in progress or all
// conflicts have been resolved and staged.
func ConflictedFiles(ctx context.Context, worktree string) ([]string, error) {
	out, err := run(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, fmt.Errorf("git: list conflicts: %w", err)
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// MergeFFOnly fast-forwards the branch checked out in dir to ref via
// `git merge --ff-only`, refusing (with a git error) if that is not
// possible. It is the fallback for [FastForward] when the target branch is
// checked out in dir itself (e.g. the repository's primary worktree), where
// the push-based update FastForward relies on cannot be used.
func MergeFFOnly(ctx context.Context, dir, ref string) error {
	// "--" keeps a ref beginning with "-" from being parsed as an option.
	if _, err := run(ctx, dir, "merge", "--ff-only", "--", ref); err != nil {
		return fmt.Errorf("git: fast-forward merge: %w", err)
	}
	return nil
}

// FastForward updates ontoBase to point at branch without checking either
// ref out, via `git push . <branch>:<ontoBase>` run in repo. Git refuses
// this both when it would not be a fast-forward and when ontoBase (or
// branch) is checked out in some worktree; those two cases are reported as
// [ErrNonFastForward] and [ErrBranchCheckedOut] respectively so callers can
// react differently (e.g. fall back to a real merge vs. surface a
// "someone's using that branch" error).
func FastForward(ctx context.Context, repo, branch, ontoBase string) error {
	// "--" keeps a refspec whose branch half begins with "-" (the
	// combined "branch:ontoBase" string starts with whatever branch
	// starts with) from being parsed as an option.
	_, err := run(ctx, repo, "push", ".", "--", branch+":"+ontoBase)
	if err == nil {
		return nil
	}

	msg := err.Error()
	switch {
	case strings.Contains(msg, "checked out"):
		return fmt.Errorf("%w: %s", ErrBranchCheckedOut, msg)
	case strings.Contains(msg, "non-fast-forward"), strings.Contains(msg, "fetch first"):
		return fmt.Errorf("%w: %s", ErrNonFastForward, msg)
	default:
		return fmt.Errorf("git: fast-forward %s onto %s: %w", branch, ontoBase, err)
	}
}

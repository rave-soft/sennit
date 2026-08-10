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
	"os"
	"os/exec"
	"strings"
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
)

// run executes git with the given args in dir and returns trimmed stdout.
// On failure the returned error wraps git's stderr so callers get an
// actionable message without having to inspect *exec.ExitError themselves.
func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Force a C locale so error-message matching (see FastForward) doesn't
	// depend on the user's system locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// IsRepo reports whether dir is inside a git working tree (or repo dir).
func IsRepo(ctx context.Context, dir string) bool {
	out, err := run(ctx, dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
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
func WorktreeAdd(ctx context.Context, repo, path, newBranch, base string) error {
	if _, err := run(ctx, repo, "worktree", "add", "-b", newBranch, path, base); err != nil {
		return fmt.Errorf("git: worktree add: %w", err)
	}
	return nil
}

// WorktreeRemove removes the worktree at path and prunes stale worktree
// metadata. If force is true, uncommitted changes and untracked files in
// the worktree are discarded without complaint.
func WorktreeRemove(ctx context.Context, repo, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := run(ctx, repo, args...); err != nil {
		return fmt.Errorf("git: worktree remove: %w", err)
	}
	if _, err := run(ctx, repo, "worktree", "prune"); err != nil {
		return fmt.Errorf("git: worktree prune: %w", err)
	}
	return nil
}

// DeleteBranch deletes the local branch name. If force is true, the branch
// is deleted even if it is not fully merged (-D instead of -d).
func DeleteBranch(ctx context.Context, repo, name string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	if _, err := run(ctx, repo, "branch", flag, name); err != nil {
		return fmt.Errorf("git: delete branch: %w", err)
	}
	return nil
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
	_, mergeErr := run(ctx, worktree, "merge", "--no-edit", ref)
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

// AbortMerge aborts an in-progress merge in worktree, restoring it to the
// pre-merge state.
func AbortMerge(ctx context.Context, worktree string) error {
	if _, err := run(ctx, worktree, "merge", "--abort"); err != nil {
		return fmt.Errorf("git: merge abort: %w", err)
	}
	return nil
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
	if _, err := run(ctx, dir, "merge", "--ff-only", ref); err != nil {
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
	_, err := run(ctx, repo, "push", ".", branch+":"+ontoBase)
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

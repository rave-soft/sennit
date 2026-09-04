package lsp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	powernap "github.com/charmbracelet/x/powernap/pkg/lsp"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/fsext"
)

// filesync tracks which documents are open in the LSP server, their
// versions, and emits the didOpen/didChange/didClose notifications that
// keep the server's view of the workspace in sync.
type filesync struct {
	// files are user-opened documents, keyed by URI. Candidate restart
	// generations never mutate this map before they are published.
	files *csync.Map[string, *OpenFileInfo]

	gen         func() *clientGeneration
	cwd         string
	name        string
	fileTypes   []string
	rootMarkers []string
	debug       bool

	// diagnostics is nil in tests that construct a filesync directly, and
	// non-nil in normal operation (Client.New wires it in right after
	// creating the store). closeVanished checks before using it, the same
	// nil-tolerant convention *Client itself uses throughout this package.
	diagnostics *diagnosticsStore
}

// OpenFileInfo contains information about an open file. Version is atomic
// because NotifyChange and RefreshOpenFiles can bump the same file at once.
type OpenFileInfo struct {
	Version atomic.Int32
	URI     protocol.DocumentURI
}

func newFileSync(gen func() *clientGeneration, cwd, name string, fileTypes, rootMarkers []string, debug bool) *filesync {
	return &filesync{
		files:       csync.NewMap[string, *OpenFileInfo](),
		gen:         gen,
		cwd:         cwd,
		name:        name,
		fileTypes:   fileTypes,
		rootMarkers: rootMarkers,
		debug:       debug,
	}
}

func (f *filesync) handlesFile(path string) bool {
	if !fsext.HasPrefix(path, f.cwd) {
		slog.Debug("File outside workspace", "name", f.name, "file", path, "workDir", f.cwd)
		return false
	}
	return handlesFiletype(f.name, f.fileTypes, path)
}

// OpenFile opens a user document. The claim on the map entry happens before
// didOpen is sent, so two concurrent callers for the same path can't both
// observe it missing and both send didOpen: GetOrSet atomically decides
// which caller's placeholder wins, and only that caller sends the
// notification. The entry is removed again if didOpen fails, so a later
// call can retry.
func (f *filesync) openFile(ctx context.Context, path string) error {
	if !f.handlesFile(path) {
		return nil
	}
	uri := string(protocol.URIFromPath(path))
	// Version is preset to 1 (didOpen's version) before the entry is
	// published, not after: a concurrent notifyChange only ever sees this
	// entry once GetOrSet has published it, so it can never race a
	// post-hoc Store here and collide with, or overwrite, didOpen's
	// version.
	candidate := &OpenFileInfo{URI: protocol.DocumentURI(uri)}
	candidate.Version.Store(1)
	info := f.files.GetOrSet(uri, func() *OpenFileInfo { return candidate })
	if info != candidate {
		return nil // Already open or being opened by another caller.
	}
	gen := f.gen()
	if err := f.didOpen(ctx, path, gen); err != nil {
		f.files.Del(uri)
		return err
	}
	// restart holds r.mu for its whole run, but openFile does not — it was
	// never meant to block on a restart in flight, only to avoid racing
	// other openFile calls for the same path. So a restart's generation
	// swap can land in the window between reading gen above and didOpen
	// returning: the file's entry survives into the new generation's
	// f.files (restart's commit only overwrites its own snapshot, never
	// deletes what openFile added concurrently), which makes IsFileOpen
	// report true, but the new generation was never sent this didOpen.
	// Detect that here and reopen on whatever generation is current now,
	// looping rather than retrying once, since another restart can race
	// the retry too.
	for {
		current := f.gen()
		if current == gen {
			return nil
		}
		if err := f.didOpen(ctx, path, current); err != nil {
			f.files.Del(uri)
			return err
		}
		gen = current
	}
}

// didOpen sends a didOpen without changing user-open bookkeeping. It is used
// by restart candidates, which are intentionally isolated until publication.
func (f *filesync) didOpen(ctx context.Context, path string, gen *clientGeneration) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}
	uri := string(protocol.URIFromPath(path))
	if err := gen.client.NotifyDidOpenTextDocument(ctx, uri, string(powernap.DetectLanguage(path)), 1, string(content)); err != nil {
		return err
	}
	return nil
}

func (f *filesync) notifyChange(ctx context.Context, path string) error {
	uri := string(protocol.URIFromPath(path))
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			f.closeVanished(ctx, uri)
		}
		return fmt.Errorf("error reading file: %w", err)
	}
	fileInfo, isOpen := f.files.Get(uri)
	if !isOpen {
		return fmt.Errorf("cannot notify change for unopened file: %s", path)
	}
	newVersion := fileInfo.Version.Add(1)
	changes := []protocol.TextDocumentContentChangeEvent{{Value: protocol.TextDocumentContentChangeWholeDocument{Text: string(content)}}}
	return f.gen().client.NotifyDidChangeTextDocument(ctx, uri, int(newVersion), changes)
}

// closeVanished cleans up bookkeeping for a file that used to be open but
// no longer exists on disk (deleted by git, another process, or a tool
// that doesn't go through this client — none of which send didClose
// themselves). Without this, didClose was only ever sent from
// closeAllFiles at a graceful shutdown: the entry lingered in f.files
// forever (IsFileOpen kept reporting true for a file that no longer
// exists), and its last-known diagnostics kept showing up in
// project_diagnostics for the rest of the session. Best-effort: the file
// is gone either way, so a failed didClose is logged, not propagated.
func (f *filesync) closeVanished(ctx context.Context, uri string) {
	gen := f.gen()
	if err := gen.client.NotifyDidCloseTextDocument(ctx, uri); err != nil {
		slog.Debug("Failed to close a vanished file", "name", f.name, "uri", uri, "error", err)
	}
	f.files.Del(uri)
	if f.diagnostics != nil {
		f.diagnostics.clearURI(gen, protocol.DocumentURI(uri))
	}
}

func (f *filesync) isFileOpen(path string) bool {
	_, exists := f.files.Get(string(protocol.URIFromPath(path)))
	return exists
}

func (f *filesync) closeAllFiles(ctx context.Context, gen *clientGeneration) {
	for uri := range f.files.Seq2() {
		if f.debug {
			slog.Debug("Closing file", "file", uri)
		}
		if err := gen.client.NotifyDidCloseTextDocument(ctx, uri); err != nil {
			slog.Warn("Error closing file", "uri", uri, "error", err)
		}
		f.files.Del(uri)
	}
}

// prepareSync snapshots the user-open files, returning a closure that
// syncs them onto a target generation once that generation is ready. A
// root marker opened for the first time is not part of this snapshot —
// there is nothing to snapshot yet — but prepareSyncOn registers it into
// f.files at commit, the same as any other file, so it is part of every
// snapshot after that. Candidate notifications remain isolated until
// publish, markers included.
//
// It serves both callers of the generation-sync path, which is why the
// name says "sync" rather than "restart": Restart, where the snapshot is
// the point (the old generation's open files must be carried across), and
// WaitForServerReady on a first boot, where no old generation exists and
// the snapshot is simply empty.
//
// The two-layer closure is intentional, not incidental: prepareSync
// itself must run — and take its userFiles snapshot — before the old
// generation is torn down, while the closure it returns runs later,
// against the new candidate generation, only once that candidate has
// initialized. Flattening the two into one call would force the snapshot
// to happen at candidate-ready time instead, after the old generation (and
// the files it had open) may already be gone.
func (f *filesync) prepareSync() func(context.Context, *clientGeneration) (func(), error) {
	userFiles := make(map[string]*OpenFileInfo)
	for uri, info := range f.files.Seq2() {
		userFiles[uri] = info
	}
	return func(ctx context.Context, gen *clientGeneration) (func(), error) {
		return f.prepareSyncOn(ctx, gen, userFiles)
	}
}

func (f *filesync) prepareSyncOn(ctx context.Context, gen *clientGeneration, userFiles map[string]*OpenFileInfo) (func(), error) {
	userURIs := make([]string, 0, len(userFiles))
	for uri := range userFiles {
		userURIs = append(userURIs, uri)
	}
	openedSet := make(map[string]struct{}, len(userURIs)+len(f.rootMarkers))
	openCandidate := func(path string) error {
		uri := string(protocol.URIFromPath(path))
		if _, exists := openedSet[uri]; exists {
			return nil
		}
		if err := f.didOpen(ctx, path, gen); err != nil {
			return err
		}
		openedSet[uri] = struct{}{}
		return nil
	}
	// vanished collects the files that no longer exist on disk. They are
	// dropped from the bookkeeping at commit time, not before: until the
	// candidate is published the current generation still has them open,
	// and a sync that fails must leave the snapshot exactly as it found
	// it.
	var vanished []string
	// markers opened this round are recorded here, not in f.files, until
	// commit: a marker is a candidate-only bootstrap document exactly like
	// a reopened user file, and must stay invisible to IsFileOpen and the
	// rest of f.files until the candidate that opened it is the published
	// generation.
	markerInfo := make(map[string]*OpenFileInfo)
	restore := func() {
		for uri, info := range userFiles {
			f.files.Set(uri, info)
		}
		// A marker is registered in f.files the same way a user file is,
		// so IsFileOpen, resyncOpenFiles, refreshOpenFiles, and
		// closeAllFiles all see it. Only set it if this generation didn't
		// already inherit it from userFiles (a marker that is also a
		// tracked user file — e.g. go.mod read or edited directly — keeps
		// whatever version userFiles carried forward instead of being
		// reset to 1 here).
		for uri, info := range markerInfo {
			if _, exists := f.files.Get(uri); !exists {
				f.files.Set(uri, info)
			}
		}
		for _, uri := range vanished {
			f.files.Del(uri)
		}
	}
	for _, marker := range f.rootMarkers {
		path := filepath.Join(f.cwd, marker)
		info, err := os.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat root marker %s: %w", path, err)
			}
			continue
		}
		// A root marker names a directory at least as often as a file:
		// ".git" is the most common marker in the whole server catalogue
		// (see powernap's lsps.json) and is a directory in every ordinary
		// checkout — only a worktree makes it a file. It marks the root
		// perfectly well, but there is nothing to didOpen, and reading it
		// fails with EISDIR. That error failed the whole sync, so
		// WaitForServerReady failed, so the client was left at
		// StateError — which reusableClient refuses to hand back, so
		// every single Start shut the server down and spawned a fresh
		// one. gopls never got to warm up in any Go repo with a plain
		// .git directory.
		if info.IsDir() {
			continue
		}
		uri := string(protocol.URIFromPath(path))
		_, alreadyOpened := openedSet[uri]
		if err := openCandidate(path); err != nil {
			return nil, fmt.Errorf("open root marker %s: %w", path, err)
		}
		if !alreadyOpened {
			newInfo := &OpenFileInfo{URI: protocol.DocumentURI(uri)}
			newInfo.Version.Store(1)
			markerInfo[uri] = newInfo
		}
	}
	for _, uri := range userURIs {
		path, err := protocol.DocumentURI(uri).Path()
		if err != nil {
			return nil, fmt.Errorf("convert reopened URI %s: %w", uri, err)
		}
		if err := openCandidate(path); err != nil {
			// A file open at the last restart can be gone by this one —
			// a branch switch, a rename, or another session deleting it
			// mid-refactor. It is not the server's fault and there is
			// nothing to reopen, but failing here failed the whole
			// restart: the candidate was rolled back and the client left
			// down, so one deleted file took gopls out for the rest of
			// the session ("Failed to restart 1 LSP client(s): gopls").
			// Drop it from the set instead and carry on with the rest.
			if errors.Is(err, fs.ErrNotExist) {
				slog.Debug("Dropping a file that no longer exists from the LSP reopen set", "name", f.name, "path", path)
				vanished = append(vanished, uri)
				continue
			}
			return nil, fmt.Errorf("reopen file %s: %w", path, err)
		}
	}
	return restore, nil
}

func (f *filesync) notifyWorkspaceChange(ctx context.Context) error {
	return f.gen().client.NotifyDidChangeWatchedFiles(ctx, []protocol.FileEvent{{URI: protocol.URIFromPath(f.cwd), Type: protocol.Changed}})
}

func (f *filesync) refreshOpenFiles(ctx context.Context) {
	for uri, info := range f.files.Seq2() {
		path, err := protocol.DocumentURI(uri).Path()
		if err != nil {
			slog.Warn("Failed to convert URI to path", "uri", uri, "error", err)
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				f.closeVanished(ctx, uri)
				continue
			}
			slog.Warn("Failed to read file for refresh", "path", path, "error", err)
			continue
		}
		version := info.Version.Add(1)
		changes := []protocol.TextDocumentContentChangeEvent{{Value: protocol.TextDocumentContentChangeWholeDocument{Text: string(content)}}}
		if err := f.gen().client.NotifyDidChangeTextDocument(ctx, uri, int(version), changes); err != nil {
			slog.Warn("Failed to notify file change", "uri", uri, "error", err)
		}
	}
}

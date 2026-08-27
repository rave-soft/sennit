package lsp

import (
	"context"
	"fmt"
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
	candidate := &OpenFileInfo{URI: protocol.DocumentURI(uri)}
	info := f.files.GetOrSet(uri, func() *OpenFileInfo { return candidate })
	if info != candidate {
		return nil // Already open or being opened by another caller.
	}
	if err := f.didOpen(ctx, path, f.gen()); err != nil {
		f.files.Del(uri)
		return err
	}
	info.Version.Store(1)
	return nil
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

// prepareRestart snapshots user-open files before the old generation is
// closed. Markers are candidate-only bootstrap documents and are never added
// to this snapshot. Candidate notifications remain isolated until publish.
func (f *filesync) prepareRestart() func(context.Context, *clientGeneration) (func(), error) {
	userFiles := make(map[string]*OpenFileInfo)
	for uri, info := range f.files.Seq2() {
		userFiles[uri] = info
	}
	return func(ctx context.Context, gen *clientGeneration) (func(), error) {
		return f.prepareRestartOn(ctx, gen, userFiles)
	}
}

func (f *filesync) prepareRestartOn(ctx context.Context, gen *clientGeneration, userFiles map[string]*OpenFileInfo) (func(), error) {
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
	restore := func() {
		for uri, info := range userFiles {
			f.files.Set(uri, info)
		}
	}
	for _, marker := range f.rootMarkers {
		path := filepath.Join(f.cwd, marker)
		if _, err := os.Stat(path); err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("stat root marker %s: %w", path, err)
			}
			continue
		}
		if err := openCandidate(path); err != nil {
			return nil, fmt.Errorf("open root marker %s: %w", path, err)
		}
	}
	for _, uri := range userURIs {
		path, err := protocol.DocumentURI(uri).Path()
		if err != nil {
			return nil, fmt.Errorf("convert reopened URI %s: %w", uri, err)
		}
		if err := openCandidate(path); err != nil {
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

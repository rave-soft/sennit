package tools

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/filetracker"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/permission"
)

type (
	sessionIDContextKey string
	messageIDContextKey string
	supportsImagesKey   string
	modelNameKey        string
	depthContextKey     string
)

const (
	// SessionIDContextKey is the key for the session ID in the context.
	SessionIDContextKey sessionIDContextKey = "session_id"
	// MessageIDContextKey is the key for the message ID in the context.
	MessageIDContextKey messageIDContextKey = "message_id"
	// SupportsImagesContextKey is the key for the model's image support capability.
	SupportsImagesContextKey supportsImagesKey = "supports_images"
	// ModelNameContextKey is the key for the model name in the context.
	ModelNameContextKey modelNameKey = "model_name"
	// DepthContextKey is the key for the current turn's background-
	// delegation cascade depth (0 for a real user turn; N+1 for a
	// continuation started from a depth-N completion). The "agent" tool's
	// background mode reads this to refuse starting further background
	// work once the cascade limit is reached — see
	// internal/agent/agent_tool.go.
	DepthContextKey depthContextKey = "depth"
)

// getContextValue is a generic helper that retrieves a typed value from context.
// If the value is not found or has the wrong type, it returns the default value.
func getContextValue[T any](ctx context.Context, key any, defaultValue T) T {
	value := ctx.Value(key)
	if value == nil {
		return defaultValue
	}
	if typedValue, ok := value.(T); ok {
		return typedValue
	}
	return defaultValue
}

// GetSessionFromContext retrieves the session ID from the context.
func GetSessionFromContext(ctx context.Context) string {
	return getContextValue(ctx, SessionIDContextKey, "")
}

// GetMessageFromContext retrieves the message ID from the context.
func GetMessageFromContext(ctx context.Context) string {
	return getContextValue(ctx, MessageIDContextKey, "")
}

// GetSupportsImagesFromContext retrieves whether the model supports images from the context.
func GetSupportsImagesFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SupportsImagesContextKey, false)
}

// GetModelNameFromContext retrieves the model name from the context.
func GetModelNameFromContext(ctx context.Context) string {
	return getContextValue(ctx, ModelNameContextKey, "")
}

// GetDepthFromContext retrieves the current turn's background-delegation
// cascade depth from the context. Absent (0) means a real user turn.
func GetDepthFromContext(ctx context.Context) int {
	return getContextValue(ctx, DepthContextKey, 0)
}

// invalidParam returns the standard tool response for a missing or invalid
// argument the model supplied. Bad input is model-recoverable — the model
// can see the message and retry with a corrected call — so it goes back as
// a normal (if IsError) tool result rather than a Go error. See the
// error-vs-response rule next to "Tools are self-documenting" in AGENTS.md.
func invalidParam(name string) fantasy.ToolResponse {
	return fantasy.NewTextErrorResponse(name + " is required")
}

// missingSessionID reports that a tool ran without a session ID bound to
// its context while performing action. The model has no way to supply
// one — it is wired in by the caller, not a tool argument — so unlike
// invalidParam this is an infrastructure failure and returns a Go error.
func missingSessionID(action string) error {
	return fmt.Errorf("session ID is required for %s", action)
}

// NewPermissionDeniedResponse returns a tool response indicating the user
// denied permission, with StopTurn set so the agent loop does not retry.
func NewPermissionDeniedResponse() fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse("User denied permission")
	resp.StopTurn = true
	return resp
}

// requirePermission requests permission for req and reports whether the
// caller must stop and return immediately. On denial it returns a plain
// "permission denied" response that callers needing to attach response
// metadata (e.g. a diff) can pass through fantasy.WithResponseMetadata
// before returning it.
func requirePermission(ctx context.Context, perms permission.Service, req permission.CreatePermissionRequest) (resp fantasy.ToolResponse, denied bool, err error) {
	granted, err := perms.Request(ctx, req)
	if err != nil {
		return fantasy.ToolResponse{}, false, err
	}
	if !granted {
		return NewPermissionDeniedResponse(), true, nil
	}
	return fantasy.ToolResponse{}, false, nil
}

// resolveWithinWorkdir resolves path to an absolute form and reports whether
// it falls outside workingDir, so callers can gate access with a permission
// request. err is non-nil only when workingDir or path cannot be resolved to
// an absolute path (e.g. an unreadable cwd).
func resolveWithinWorkdir(workingDir, path string) (absPath string, outside bool, err error) {
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", false, fmt.Errorf("resolving working directory: %w", err)
	}
	absPath, err = filepath.Abs(path)
	if err != nil {
		return "", false, fmt.Errorf("resolving path: %w", err)
	}
	relPath, err := filepath.Rel(absWorkingDir, absPath)
	outside = err != nil || strings.HasPrefix(relPath, "..")
	return absPath, outside, nil
}

// ensureParentDir creates the parent directory of filePath, as needed before
// writing a new file.
func ensureParentDir(filePath string) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}
	return nil
}

// writeFileWithHistory writes newContent to filePath and updates file
// history: it records a history entry if none exists yet, snapshots any
// content that changed on disk outside of Sennit, and stores the new
// version. Used by write/edit/multiedit whenever a tool commits file
// content, whether creating the file (oldContent == "") or overwriting it.
func writeFileWithHistory(ctx context.Context, files history.Service, sessionID, filePath, oldContent, newContent string) error {
	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	file, err := files.GetByPathAndSession(ctx, filePath, sessionID)
	switch {
	case err != nil:
		// Nothing recorded for this path in this session yet: the content
		// that was on disk before this write becomes the baseline. The
		// two branches are exclusive — writing both would store
		// oldContent twice in a row.
		if _, err := files.CreateVersion(ctx, sessionID, filePath, oldContent); err != nil {
			return fmt.Errorf("error creating file history: %w", err)
		}
	case file.Content != oldContent:
		// User manually changed the content; store an intermediate version.
		if _, err := files.CreateVersion(ctx, sessionID, filePath, oldContent); err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}
	if _, err := files.CreateVersion(ctx, sessionID, filePath, newContent); err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	return nil
}

// recordWholeFileRead marks the file as fully seen by this session. Only
// for writes where the caller supplied the entire content (Write, and
// creating a file through Edit/Multi-Edit): it saw everything it just
// wrote.
func recordWholeFileRead(ctx context.Context, filetracker filetracker.Service, sessionID, filePath string) {
	filetracker.RecordRead(ctx, sessionID, filePath)
}

// recordEditedSpan marks the lines an edit replaced as seen, and nothing
// else. An edit is not a read: the agent supplied old_string and a
// replacement, not the file. Recording a full read here would hand back
// exactly the blind-edit hole that read-coverage exists to close — read
// fifty lines, edit one of them, and the rest of the file would count as
// read from then on.
//
// oldContent/newContent must both be in the tool's normalized (LF) form,
// so the span matches the line numbers the read tool reported.
func recordEditedSpan(ctx context.Context, filetracker filetracker.Service, sessionID, filePath, oldContent, newContent string) {
	start, end, ok := changedLineSpan(oldContent, newContent)
	if !ok {
		return
	}
	delta := strings.Count(newContent, "\n") - strings.Count(oldContent, "\n")
	filetracker.RecordEdit(ctx, sessionID, filePath, start, end, max(start, end+delta))
}

// newHTTPClient builds an http.Client tuned for outbound fetch/download
// tools: a shared transport with modest idle-connection pooling, and the
// given overall request timeout (fetch and download use different
// timeouts; web_fetch reuses fetch's).
func newHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// ghAvailable indicates whether the `gh` CLI is available on PATH.
var ghAvailable = func() bool {
	if testing.Testing() {
		return false
	}
	_, err := exec.LookPath("gh")
	return err == nil
}()

// toolDescriptionData is the common data structure for tool description templates.
type toolDescriptionData struct {
	GhAvailable bool
}

// renderToolDescription renders a tool description template with the given data.
func renderToolDescription(tmpl *template.Template) string {
	data := toolDescriptionData{
		GhAvailable: ghAvailable,
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// renderTemplate renders a Go template with the given data.
func renderTemplate(tmpl *template.Template, data any) string {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// confinementRefusal reports whether filePath falls outside the boundary a
// confined workspace may write within, and the message to return if it
// does.
//
// A thread works in its own git worktree on its own branch so that its
// changes stay its own until they are merged. An absolute path is all it
// takes to step outside that: the file tools join a relative path onto the
// working directory but pass an absolute one straight through, so a model
// that has learned the project's real path — from a goal, a log line, its
// own earlier session — writes into the main checkout and the thread's
// branch stays empty. That is not a permission question, and under yolo
// (which threads inherit) no question would be asked anyway. So a confined
// workspace refuses outright, and says where the file should have gone.
//
// Returns ok=false when there is nothing to refuse: an unconfined
// workspace, or a path already inside the boundary.
func confinementRefusal(permissions permission.Service, filePath string) (message string, ok bool) {
	if permissions == nil {
		return "", false
	}
	dir := permissions.ConfinedDir()
	if dir == "" {
		return "", false
	}
	abs, outside, err := resolveWithinWorkdir(dir, filePath)
	if err != nil {
		// A path that cannot be resolved cannot be shown to be inside the
		// boundary; a confinement check has to fail closed.
		return fmt.Sprintf(
			"refusing to write outside this workspace: cannot resolve %s: %s. "+
				"This workspace is isolated to %s.",
			filePath, err, dir,
		), true
	}
	if !outside {
		return "", false
	}
	return fmt.Sprintf(
		"refusing to write outside this workspace: %s is not inside %s. "+
			"This workspace is isolated — write to the copy of the file under %s instead.",
		abs, dir, dir,
	), true
}

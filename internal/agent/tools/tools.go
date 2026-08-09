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
	"github.com/rave-soft/braid/internal/filetracker"
	"github.com/rave-soft/braid/internal/history"
	"github.com/rave-soft/braid/internal/permission"
)

type (
	sessionIDContextKey string
	messageIDContextKey string
	supportsImagesKey   string
	modelNameKey        string
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
// content that changed on disk outside of Braid, and stores the new
// version. Used by write/edit/multiedit whenever a tool commits file
// content, whether creating the file (oldContent == "") or overwriting it.
func writeFileWithHistory(ctx context.Context, files history.Service, filetracker filetracker.Service, sessionID, filePath, oldContent, newContent string) error {
	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	file, err := files.GetByPathAndSession(ctx, filePath, sessionID)
	if err != nil {
		if _, err := files.Create(ctx, sessionID, filePath, oldContent); err != nil {
			return fmt.Errorf("error creating file history: %w", err)
		}
	}
	if file.Content != oldContent {
		// User manually changed the content; store an intermediate version.
		if _, err := files.CreateVersion(ctx, sessionID, filePath, oldContent); err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}
	if _, err := files.CreateVersion(ctx, sessionID, filePath, newContent); err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	filetracker.RecordRead(ctx, sessionID, filePath)
	return nil
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

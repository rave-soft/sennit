package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/brand"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
)

type DownloadParams struct {
	URL      string `json:"url" description:"The URL to download from"`
	FilePath string `json:"file_path" description:"The local file path where the downloaded content should be saved"`
	Timeout  int    `json:"timeout,omitempty" description:"Optional timeout in seconds (max 600)"`
}

// DownloadPermissionsParams is defined in proto; see the comment on
// BashPermissionsParams in bash.go.
type DownloadPermissionsParams = proto.DownloadPermissionsParams

const DownloadToolName = "download"

const (
	// MaxDownloadSize limits downloads so an untrusted response cannot fill the disk.
	MaxDownloadSize = 100 * 1024 * 1024 // 100MB

	// defaultDownloadTimeout bounds a download call that didn't specify a
	// timeout, now that the http.Client itself carries none - see NewDownloadTool.
	defaultDownloadTimeout = 5 * time.Minute
)

//go:embed download.md.tpl
var downloadDescriptionTmpl []byte

var downloadDescriptionTpl = template.Must(
	template.New("downloadDescription").
		Parse(string(downloadDescriptionTmpl)),
)

type downloadDescriptionData struct {
	MaxDownloadSizeMB  int
	MaxDownloadTimeout int
}

func downloadDescription() string {
	return renderTemplate(downloadDescriptionTpl, downloadDescriptionData{
		MaxDownloadSizeMB:  MaxDownloadSize / (1024 * 1024),
		MaxDownloadTimeout: 600,
	})
}

func NewDownloadTool(permissions permission.Requester, workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		// No client.Timeout here: it would bound the whole request
		// regardless of the caller-supplied timeout below, capping the
		// documented 600s maximum at whatever this constant said. The
		// per-call context timeout is the only bound now; defaultDownloadTimeout
		// keeps an unspecified timeout from hanging forever.
		client = NewHTTPClient(0)
	}
	return withToolParameterSchema(fantasy.NewAgentTool(
		DownloadToolName,
		downloadDescription(),
		func(ctx context.Context, params DownloadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return invalidParam("url"), nil
			}

			if params.FilePath == "" {
				return invalidParam("file_path"), nil
			}

			if params.Timeout < 0 || params.Timeout > 600 {
				return fantasy.NewTextErrorResponse("timeout must be between 0 and 600 seconds"), nil
			}

			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return fantasy.NewTextErrorResponse("URL must start with http:// or https://"), nil
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)
			relPath, _ := filepath.Rel(workingDir, filePath)
			relPath = filepath.ToSlash(cmp.Or(relPath, filePath))

			if msg, refused := confinementRefusal(permissions, filePath); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("downloading files")
			}

			// Resolve the parent, never the leaf. An ancestor directory
			// symlink (`ln -s ../.. up`, then `download <url> up/x`)
			// leaves filePath's string form inside workingDir while the
			// write lands wherever the link points, so the dialog has to
			// name that destination. A leaf symlink is the opposite case:
			// fsext.ReplaceFile renames onto filePath itself and so
			// replaces the link rather than following it (the guarantee
			// TestDownloadTool_DoesNotFollowSymlinkOutOfWorkspace pins),
			// and naming its target would label the dialog with a file
			// this download never writes — and key a persistent grant on
			// it, quietly pre-approving a later download that really does
			// target it.
			parent, base := filepath.Split(filePath)
			_, resolvedParent, _, err := resolveWithinWorkdir(workingDir, filepath.Clean(parent))
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			resolvedFilePath := filepath.Join(resolvedParent, base)
			description := fmt.Sprintf("Download file from URL: %s to %s", params.URL, filePath)
			if resolvedFilePath != filePath {
				description = fmt.Sprintf("%s (resolves to %s)", description, resolvedFilePath)
			}

			permResp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  call.ID,
				Path:        resolvedFilePath,
				ToolName:    DownloadToolName,
				Action:      "download",
				Description: description,
				Params:      DownloadPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if denied {
				return permResp, nil
			}

			// Handle timeout with context. The client itself carries no
			// Timeout (see NewDownloadTool), so this is the only thing
			// bounding the request; an unspecified timeout still falls back
			// to defaultDownloadTimeout rather than being allowed to hang
			// forever.
			requestTimeout := defaultDownloadTimeout
			if params.Timeout > 0 {
				requestTimeout = time.Duration(params.Timeout) * time.Second
			}
			requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()

			// A malformed URL or an unreachable target is information about
			// what the model asked for, not about this process, so both
			// come back as a normal tool result the model can react to
			// (e.g. by trying a different URL) — matching fetch/web_fetch's
			// handling of the same failure. Local filesystem failures below
			// (creating directories, the output file, writing) stay Go
			// errors: those are about this process, not the URL.
			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to create request: %s", err)), nil
			}

			req.Header.Set("User-Agent", brand.Slug+"/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Failed to download from URL: %s", err)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}
			if resp.ContentLength > MaxDownloadSize {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Download exceeds the maximum size of %d bytes", MaxDownloadSize)), nil
			}

			// Create parent directories if they don't exist
			destDir := filepath.Dir(filePath)
			if err := os.MkdirAll(destDir, 0o755); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
			}

			// Write to a temp file in the destination directory rather than
			// os.Create(filePath) directly, for two reasons: os.Create
			// follows an existing symlink at filePath and truncates
			// whatever it points at (a pre-existing or bash-created link
			// out of a confined workspace would let a download clobber a
			// file this tool was never granted access to), and truncating
			// in place destroys a complete existing file the moment a
			// download starts, even if it then fails or is cancelled
			// partway through. Renaming a fully-copied temp file into place
			// avoids both: the temp file is created fresh (never following
			// a link), and filePath is only touched once the copy has
			// succeeded.
			outFile, err := os.CreateTemp(destDir, filepath.Base(filePath)+".*.tmp")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to create output file: %w", err)
			}
			tmpPath := outFile.Name()
			cleanupTmp := true
			defer func() {
				outFile.Close()
				if cleanupTmp {
					_ = os.Remove(tmpPath)
				}
			}()

			// Read one byte past the cap to distinguish a body exactly at the
			// limit from one that exceeds it. The temporary file is removed by
			// the deferred cleanup on every failure, leaving an existing target
			// untouched.
			bytesWritten, err := io.Copy(outFile, io.LimitReader(resp.Body, MaxDownloadSize+1))
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
			}
			if bytesWritten > MaxDownloadSize {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Download exceeds the maximum size of %d bytes", MaxDownloadSize)), nil
			}
			if err := outFile.Close(); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
			}
			if err := fsext.ReplaceFile(tmpPath, filePath); err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
			}
			cleanupTmp = false

			contentType := resp.Header.Get("Content-Type")
			responseMsg := fmt.Sprintf("Successfully downloaded %d bytes to %s", bytesWritten, relPath)
			if contentType != "" {
				responseMsg += fmt.Sprintf(" (Content-Type: %s)", contentType)
			}

			return fantasy.NewTextResponse(responseMsg), nil
		},
	), map[string]toolParameterSchema{"url": {minLength: intPtr(1)}, "file_path": {minLength: intPtr(1)}, "timeout": intSchemaBounds(600)})
}

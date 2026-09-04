package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/x/powernap/pkg/lsp/protocol"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/lsp"
)

type DiagnosticsParams struct {
	FilePath string `json:"file_path,omitempty" description:"The path to the file to get diagnostics for (leave empty for project diagnostics)"`
}

const DiagnosticsToolName = "lsp_diagnostics"

//go:embed diagnostics.md
var diagnosticsDescription string

// noLSPRunningMessage is returned by the tool (not by getDiagnostics,
// which other callers rely on staying silent when there's nothing to
// report — see finishMutation) when no LSP client covers what was asked
// for at all. An empty result otherwise reads identically whether nothing
// was wrong or nothing could be checked; a model that treats "" as
// "clean" then misses every error a configured-but-not-yet-started
// server would have found.
const noLSPRunningMessage = "No LSP client is running; diagnostics could not be checked."

// noLSPReadyMessage is returned when a client covers the request but has
// not finished its initialize handshake yet (GetServerState() is not yet
// StateReady) — distinct from noLSPRunningMessage: a client that exists
// but isn't ready can't have published anything, so getDiagnostics below
// would come back empty and read exactly like a genuinely clean file.
const noLSPReadyMessage = "LSP client for this file is still starting; diagnostics could not be checked yet."

// noDiagnosticsFoundMessage confirms diagnostics were actually checked
// and came back empty, as distinct from noLSPRunningMessage and
// noLSPReadyMessage above. It does not by itself guarantee a server that
// indexes asynchronously after reporting ready has finished doing so —
// see diagnostics.md.
const noDiagnosticsFoundMessage = "No diagnostics found."

func NewDiagnosticsTool(lspManager *lsp.Manager, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		DiagnosticsToolName,
		diagnosticsDescription,
		func(ctx context.Context, params DiagnosticsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			filePath := params.FilePath
			if filePath != "" {
				filePath = filepathext.SmartJoin(workingDir, filePath)
			}
			notifyLSPs(ctx, lspManager, filePath)
			if lspManager == nil {
				return fantasy.NewTextResponse(noLSPRunningMessage), nil
			}
			clients := clientsForDiagnostics(lspManager, filePath)
			if len(clients) == 0 {
				return fantasy.NewTextResponse(noLSPRunningMessage), nil
			}
			if !anyClientReady(clients) {
				return fantasy.NewTextResponse(noLSPReadyMessage), nil
			}
			output := getDiagnostics(filePath, lspManager)
			if output == "" {
				output = noDiagnosticsFoundMessage
			}
			return fantasy.NewTextResponse(output), nil
		},
	)
}

// clientsForDiagnostics returns the clients relevant to a diagnostics
// request: every client that handles filePath, or every registered
// client when filePath is empty (project-wide diagnostics span all of
// them). Checking coverage this way, per file, is what tells "only a
// Python server is running and this is a Go file" apart from "nothing is
// running" — lspManager.Clients().Len() alone can't, since it counts
// clients for languages that have nothing to do with the file asked
// about.
func clientsForDiagnostics(manager *lsp.Manager, filePath string) []*lsp.Client {
	var clients []*lsp.Client
	for c := range manager.Clients().Seq() {
		if filePath == "" || c.HandlesFile(filePath) {
			clients = append(clients, c)
		}
	}
	return clients
}

// anyClientReady reports whether at least one of clients has finished its
// initialize handshake. A client still in StateStarting has not had a
// chance to publish anything, so its silence must not be read as "clean".
func anyClientReady(clients []*lsp.Client) bool {
	for _, c := range clients {
		if c.GetServerState() == lsp.StateReady {
			return true
		}
	}
	return false
}

// openInLSPs ensures LSP servers are running, aware of the file, and
// resynced against what is actually on disk, but does not wait for fresh
// diagnostics itself — the caller does that (read uses a short timeout;
// see waitForLSPDiagnostics). OpenFileOnDemand alone only opens a file a
// client has never seen; once open it is a no-op, so a file whose overlay
// went stale after a read or bash tool touched the file on disk (neither
// sends didChange) would otherwise stay stale, and the diagnostics printed
// alongside the freshly read content would describe an older version of
// the file. NotifyChange re-reads and re-syncs it, and itself errors on a
// file the client has never opened, so OpenFileOnDemand must run first.
func openInLSPs(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
) {
	if filepath == "" || manager == nil {
		return
	}

	manager.Start(ctx, filepath)

	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		if err := syncOverlay(ctx, client, filepath); err != nil {
			slog.DebugContext(ctx, "Failed to sync overlay before reading", "path", filepath, "error", err)
		}
	}
}

// syncOverlay brings client's overlay for path in line with what is
// actually on disk before a position- or content-sensitive request goes
// out. Every client.<Request> method already calls OpenFileOnDemand
// internally (see requests.go's ensureOpen), but that alone only opens a
// file the client has never seen — once open it is a no-op, so a file
// whose overlay went stale (a read or bash tool changed it on disk without
// going through edit/write/multiedit, none of which notify this client)
// stays stale indefinitely. NotifyChange re-reads and re-syncs it, and
// itself errors on a file the client has never opened, so the two calls
// must run in this order.
func syncOverlay(ctx context.Context, client *lsp.Client, path string) error {
	if err := client.OpenFileOnDemand(ctx, path); err != nil {
		return err
	}
	return client.NotifyChange(ctx, path)
}

// waitForLSPDiagnostics waits briefly for diagnostics publication after a file
// has been opened. Intended for read-only situations where viewing up-to-date
// files matters but latency should remain low (i.e. when using the read tool).
func waitForLSPDiagnostics(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
	timeout time.Duration,
) {
	if filepath == "" || manager == nil || timeout <= 0 {
		return
	}

	var wg sync.WaitGroup
	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		wg.Go(func() {
			client.WaitForDiagnostics(ctx, timeout)
		})
	}
	wg.Wait()
}

// notifyLSPs notifies LSP servers that a file has changed and waits for
// updated diagnostics. Use this after edit/multiedit operations.
// When filepath is empty, refreshes all open files across all LSP clients
// and sends a workspace-level change notification for full re-analysis.
func notifyLSPs(
	ctx context.Context,
	manager *lsp.Manager,
	filepath string,
) {
	if manager == nil {
		return
	}
	if filepath == "" {
		// No specific file — refresh all open files for all clients.
		var wg sync.WaitGroup
		for client := range manager.Clients().Seq() {
			wg.Go(func() {
				client.RefreshOpenFiles(ctx)
				if err := client.NotifyWorkspaceChange(ctx); err != nil {
					slog.WarnContext(ctx, "Failed to notify workspace change", "error", err)
				}
				client.WaitForDiagnostics(ctx, 5*time.Second)
			})
		}
		wg.Wait()
		return
	}

	manager.Start(ctx, filepath)

	var wg sync.WaitGroup
	for client := range manager.Clients().Seq() {
		if !client.HandlesFile(filepath) {
			continue
		}
		_ = client.OpenFileOnDemand(ctx, filepath)
		_ = client.NotifyChange(ctx, filepath)
		wg.Go(func() {
			client.WaitForDiagnostics(ctx, 5*time.Second)
		})
	}
	wg.Wait()
}

func getDiagnostics(filePath string, manager *lsp.Manager) string {
	if manager == nil {
		return ""
	}

	var fileDiagnostics []string
	var projectDiagnostics []string

	for lspName, client := range manager.Clients().Seq2() {
		for location, diags := range client.GetDiagnostics() {
			path, err := location.Path()
			if err != nil {
				slog.Error("Failed to convert diagnostic location URI to path", "uri", location, "error", err)
				continue
			}
			isCurrentFile := path == filePath
			for _, diag := range diags {
				formattedDiag := formatDiagnostic(path, diag, lspName)
				if isCurrentFile {
					fileDiagnostics = append(fileDiagnostics, formattedDiag)
				} else {
					projectDiagnostics = append(projectDiagnostics, formattedDiag)
				}
			}
		}
	}

	sortDiagnostics(fileDiagnostics)
	sortDiagnostics(projectDiagnostics)

	var output strings.Builder
	writeDiagnostics(&output, "file_diagnostics", fileDiagnostics)
	writeDiagnostics(&output, "project_diagnostics", projectDiagnostics)

	if len(fileDiagnostics) > 0 || len(projectDiagnostics) > 0 {
		fileErrors := countSeverity(fileDiagnostics, "Error")
		fileWarnings := countSeverity(fileDiagnostics, "Warn")
		projectErrors := countSeverity(projectDiagnostics, "Error")
		projectWarnings := countSeverity(projectDiagnostics, "Warn")
		output.WriteString("\n<diagnostic_summary>\n")
		fmt.Fprintf(&output, "Current file: %d errors, %d warnings\n", fileErrors, fileWarnings)
		fmt.Fprintf(&output, "Project: %d errors, %d warnings\n", projectErrors, projectWarnings)
		output.WriteString("</diagnostic_summary>\n")
	}

	out := output.String()
	slog.Debug("Diagnostics", "output", out)
	return out
}

func writeDiagnostics(output *strings.Builder, tag string, in []string) {
	if len(in) == 0 {
		return
	}
	output.WriteString("\n<" + tag + ">\n")
	if len(in) > 10 {
		output.WriteString(strings.Join(in[:10], "\n"))
		fmt.Fprintf(output, "\n... and %d more diagnostics", len(in)-10)
	} else {
		output.WriteString(strings.Join(in, "\n"))
	}
	output.WriteString("\n</" + tag + ">\n")
}

func sortDiagnostics(in []string) {
	sort.Slice(in, func(i, j int) bool {
		iIsError := strings.HasPrefix(in[i], "Error")
		jIsError := strings.HasPrefix(in[j], "Error")
		if iIsError != jIsError {
			return iIsError // Errors come first
		}
		return in[i] < in[j] // Then alphabetically
	})
}

func formatDiagnostic(pth string, diagnostic protocol.Diagnostic, source string) string {
	severity := "Info"
	switch diagnostic.Severity {
	case protocol.SeverityError:
		severity = "Error"
	case protocol.SeverityWarning:
		severity = "Warn"
	case protocol.SeverityHint:
		severity = "Hint"
	}

	location := fmt.Sprintf("%s:%d:%d", pth, diagnostic.Range.Start.Line+1, diagnostic.Range.Start.Character+1)

	sourceInfo := source
	if diagnostic.Source != "" {
		sourceInfo += " " + diagnostic.Source
	}

	codeInfo := ""
	if diagnostic.Code != nil {
		codeInfo = fmt.Sprintf("[%v]", diagnostic.Code)
	}

	tagsInfo := ""
	if len(diagnostic.Tags) > 0 {
		var tags []string
		for _, tag := range diagnostic.Tags {
			switch tag {
			case protocol.Unnecessary:
				tags = append(tags, "unnecessary")
			case protocol.Deprecated:
				tags = append(tags, "deprecated")
			}
		}
		if len(tags) > 0 {
			tagsInfo = fmt.Sprintf(" (%s)", strings.Join(tags, ", "))
		}
	}

	return fmt.Sprintf("%s: %s [%s]%s%s %s",
		severity,
		location,
		sourceInfo,
		codeInfo,
		tagsInfo,
		diagnostic.Message)
}

func countSeverity(diagnostics []string, severity string) int {
	count := 0
	for _, diag := range diagnostics {
		if strings.HasPrefix(diag, severity) {
			count++
		}
	}
	return count
}

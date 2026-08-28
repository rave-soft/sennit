package tools

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"charm.land/fantasy"
	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/filepathext"
	"github.com/rave-soft/sennit/internal/fsext"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/shell"
)

type BashParams struct {
	Description         string `json:"description" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	Command             string `json:"command" description:"The command to execute"`
	WorkingDir          string `json:"working_dir,omitempty" description:"The working directory to execute the command in (defaults to current directory)"`
	RunInBackground     bool   `json:"run_in_background,omitempty" description:"Set to true (boolean) to run this command in the background. Use job_output to read the output later."`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty" description:"Seconds to wait before automatically moving the command to a background job (default: 60, max: 600)"`
}

type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
}

type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
	Background       bool   `json:"background,omitempty"`
	ShellID          string `json:"shell_id,omitempty"`
}

const (
	BashToolName = "bash"

	DefaultAutoBackgroundAfter = 60 // Commands taking longer automatically become background jobs

	// MaxAutoBackgroundAfter bounds how long a model can stretch the
	// auto-background wait. Auto-background exists so a slow command
	// cannot eat an entire turn; a model asking for, say, a day-long
	// window would defeat that purpose. Ten minutes is generous headroom
	// over the default while still keeping a hard ceiling.
	MaxAutoBackgroundAfter = 600

	MaxOutputLength = 30000
	BashNoOutput    = "no output"
)

//go:embed bash.md.tpl
var bashDescriptionTmpl []byte

var bashDescriptionTpl = template.Must(
	template.New("bashDescription").
		Parse(string(bashDescriptionTmpl)),
)

type bashDescriptionData struct {
	BannedCommands  string
	MaxOutputLength int
	Attribution     config.Attribution
	ModelID         string
	RgAvailable     bool
	GhAvailable     bool
}

var bannedCommands = []string{
	// Network/Download tools
	"alias",
	"aria2c",
	"axel",
	"chrome",
	"curl",
	"curlie",
	"firefox",
	"http-prompt",
	"httpie",
	"links",
	"lynx",
	"nc",
	"safari",
	"scp",
	"ssh",
	"telnet",
	"w3m",
	"wget",
	"xh",

	// System administration
	"doas",
	"su",
	"sudo",

	// Package managers
	"apk",
	"apt",
	"apt-cache",
	"apt-get",
	"dnf",
	"dpkg",
	"emerge",
	"home-manager",
	"makepkg",
	"opkg",
	"pacman",
	"paru",
	"pkg",
	"pkg_add",
	"pkg_delete",
	"portage",
	"rpm",
	"yay",
	"yum",
	"zypper",

	// System modification
	"at",
	"batch",
	"chkconfig",
	"crontab",
	"fdisk",
	"mkfs",
	"mount",
	"parted",
	"service",
	"systemctl",
	"umount",

	// Network configuration
	"firewall-cmd",
	"ifconfig",
	"ip",
	"iptables",
	"netstat",
	"pfctl",
	"route",
	"ufw",
}

func bashDescription(attribution *config.Attribution, modelID string, availability toolAvailability) string {
	bannedCommandsStr := strings.Join(bannedCommands, ", ")
	var out bytes.Buffer
	if err := bashDescriptionTpl.Execute(&out, bashDescriptionData{
		BannedCommands:  bannedCommandsStr,
		MaxOutputLength: MaxOutputLength,
		Attribution:     *attribution,
		ModelID:         modelID,
		RgAvailable:     getRg() != "",
		GhAvailable:     availability.ghAvailable,
	}); err != nil {
		// this should never happen.
		panic("failed to execute bash description template: " + err.Error())
	}
	return out.String()
}

func blockFuncs() []shell.BlockFunc {
	return []shell.BlockFunc{
		shell.CommandsBlocker(bannedCommands),

		// System package managers
		shell.ArgumentsBlocker("apk", []string{"add"}, nil),
		shell.ArgumentsBlocker("apt", []string{"install"}, nil),
		shell.ArgumentsBlocker("apt-get", []string{"install"}, nil),
		shell.ArgumentsBlocker("dnf", []string{"install"}, nil),
		shell.ArgumentsBlocker("pacman", nil, []string{"-S"}),
		shell.ArgumentsBlocker("pkg", []string{"install"}, nil),
		shell.ArgumentsBlocker("yum", []string{"install"}, nil),
		shell.ArgumentsBlocker("zypper", []string{"install"}, nil),

		// Language-specific package managers
		shell.ArgumentsBlocker("brew", []string{"install"}, nil),
		shell.ArgumentsBlocker("cargo", []string{"install"}, nil),
		shell.ArgumentsBlocker("gem", []string{"install"}, nil),
		shell.ArgumentsBlocker("go", []string{"install"}, nil),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"--global"}),
		shell.ArgumentsBlocker("npm", []string{"install"}, []string{"-g"}),
		shell.ArgumentsBlocker("pip", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pip3", []string{"install"}, []string{"--user"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"--global"}),
		shell.ArgumentsBlocker("pnpm", []string{"add"}, []string{"-g"}),
		shell.ArgumentsBlocker("yarn", []string{"global", "add"}, nil),

		// `go test -exec` can run arbitrary commands
		shell.ArgumentsBlocker("go", []string{"test"}, []string{"-exec"}),
	}
}

// NewBashTool builds the bash tool. bgManager must be non-nil; the
// coordinator's constructor already rejects a nil BackgroundShells before any
// tool is built (see NewCoordinator's errBackgroundShellsRequired check), so
// this path is unreachable in production and callers no longer need a panic
// guard here.
func NewBashTool(permissions permission.Requester, workingDir string, attribution *config.Attribution, modelID string, bgManager *shell.BackgroundShellManager, options ...toolAvailabilityOption) fantasy.AgentTool {
	availability := applyToolAvailability(options)
	return withToolParameterSchema(fantasy.NewAgentTool(
		BashToolName,
		bashDescription(attribution, modelID, availability),
		func(ctx context.Context, params BashParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Command == "" {
				return invalidParam("command"), nil
			}

			// Determine working directory. A relative working_dir is
			// relative to the workspace, not to the process cwd — same as
			// every file tool.
			execWorkingDir := workingDir
			if params.WorkingDir != "" {
				execWorkingDir = filepathext.SmartJoin(workingDir, params.WorkingDir)
			}

			// A confined workspace refuses to run commands rooted outside
			// it. This does not confine what the command itself touches —
			// a shell can write anywhere — but it keeps the obvious
			// front door shut and the permission key canonical.
			if msg, refused := confinementRefusal(permissions, execWorkingDir); refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			// The working-dir check only shuts the front door; the command
			// text itself can still name an absolute path outside the
			// boundary. Catch what a static AST parse can see and refuse
			// outright — see bashConfinementRefusal's doc comment for
			// exactly what this does and does not catch.
			msg, refused, permissionRequired := bashConfinementRefusal(permissions, params.Command)
			if refused {
				return fantasy.NewTextErrorResponse(msg), nil
			}

			isSafeReadOnly := false
			cmdLower := strings.ToLower(params.Command)

			if !containsCommandChaining(params.Command) {
				for _, safe := range safeCommands {
					if strings.HasPrefix(cmdLower, safe) {
						if len(cmdLower) == len(safe) || cmdLower[len(safe)] == ' ' || cmdLower[len(safe)] == '-' {
							isSafeReadOnly = true
							break
						}
					}
				}
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, missingSessionID("executing shell command")
			}
			if !isSafeReadOnly || permissionRequired {
				description := fmt.Sprintf("Execute command: %s", params.Command)
				if permissionRequired {
					description += " (workspace path check is best-effort; dynamic shell expansion requires approval)"
				}
				resp, denied, err := requirePermission(ctx, permissions, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        execWorkingDir,
					ToolCallID:  call.ID,
					ToolName:    BashToolName,
					Action:      "execute",
					Description: description,
					Params:      BashPermissionsParams(params),
				})
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if denied {
					return resp, nil
				}
			}

			// If explicitly requested as background, start immediately with detached context
			if params.RunInBackground {
				startTime := time.Now()
				bgManager.Cleanup()
				// Use background context so it continues after tool returns
				bgShell, err := bgManager.Start(context.Background(), execWorkingDir, blockFuncs(), params.Command, params.Description)
				if err != nil {
					return fantasy.ToolResponse{}, fmt.Errorf("error starting background shell: %w", err)
				}

				// Wait a short time to detect fast failures (blocked commands, syntax errors, etc.),
				// but return as soon as the shell reports completion instead of always paying
				// the full second.
				select {
				case <-bgShell.Done():
				case <-time.After(1 * time.Second):
				}
				stdout, stderr, done, execErr := bgShell.GetOutput()

				if done {
					// Command failed or completed very quickly
					_ = bgManager.Remove(bgShell.ID) // shell already finished; nothing to clean up on failure
					return finishBashRun(bgShell, params, startTime, stdout, stderr, execErr)
				}

				// Still running after fast-failure check - return as background job
				return stillRunningBashResponse(bgShell, params, startTime,
					"Background shell started with ID: %s\n\nUse job_output tool to view output or job_kill to terminate."), nil
			}

			// Start synchronous execution with auto-background support
			startTime := time.Now()

			// Start with detached context so it can survive if moved to background
			bgManager.Cleanup()
			bgShell, err := bgManager.Start(context.Background(), execWorkingDir, blockFuncs(), params.Command, params.Description)
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("error starting shell: %w", err)
			}

			// Wait for either completion, auto-background threshold, or context cancellation.
			// bgShell.Done() is closed exactly once, after exitErr/completedAt are recorded,
			// so there is no need to poll GetOutput on a ticker.
			autoBackgroundAfter := normalizeAutoBackgroundAfter(params.AutoBackgroundAfter)
			autoBackgroundThreshold := time.Duration(autoBackgroundAfter) * time.Second
			timeout := time.After(autoBackgroundThreshold)

			var stdout, stderr string
			var done bool
			var execErr error

			select {
			case <-bgShell.Done():
				stdout, stderr, done, execErr = bgShell.GetOutput()
			case <-timeout:
				stdout, stderr, done, execErr = bgShell.GetOutput()
			case <-ctx.Done():
				// Incoming context was cancelled before we moved to background
				// Kill the shell and return error
				_ = bgManager.Kill(bgShell.ID) // best-effort; ctx.Err() below is what we report
				return fantasy.ToolResponse{}, ctx.Err()
			}

			if done {
				// Command completed within threshold - return synchronously
				// Remove from background manager since we're returning directly
				// Don't call Kill() as it cancels the context and corrupts the exit code
				_ = bgManager.Remove(bgShell.ID) // shell already finished; nothing to clean up
				return finishBashRun(bgShell, params, startTime, stdout, stderr, execErr)
			}

			// Still running - keep as background job
			return stillRunningBashResponse(bgShell, params, startTime,
				"Command is taking longer than expected and has been moved to background.\n\nBackground shell ID: %s\n\nUse job_output tool to view output or job_kill to terminate."), nil
		},
	), map[string]toolParameterSchema{"command": {minLength: intPtr(1)}, "auto_background_after": intSchemaBounds(MaxAutoBackgroundAfter)})
}

// normalizeAutoBackgroundAfter clamps a model-supplied auto-background
// threshold into a sane range. The model is not a trusted source: zero or
// negative values (including an omitted field, which decodes as zero) mean
// "not set" and fall back to the default, and anything above
// MaxAutoBackgroundAfter is capped there so a command can never be made to
// run synchronously forever.
func normalizeAutoBackgroundAfter(seconds int) int {
	if seconds <= 0 {
		return DefaultAutoBackgroundAfter
	}
	if seconds > MaxAutoBackgroundAfter {
		return MaxAutoBackgroundAfter
	}
	return seconds
}

// finishBashRun turns a finished shell run's raw output into the tool's
// response: a Go error for a genuine execution failure (so the whole
// tool-call batch aborts), or a text response carrying
// BashResponseMetadata otherwise. Shared by the run-in-background path's
// fast-failure check and the synchronous path's within-threshold
// completion — both call this once bgShell.GetOutput() reports done.
func finishBashRun(bgShell *shell.BackgroundShell, params BashParams, startTime time.Time, stdout, stderr string, execErr error) (fantasy.ToolResponse, error) {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)
	if exitCode == 0 && !interrupted && execErr != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", bgShell.ID, execErr)
	}

	stdout = formatOutput(stdout, stderr, execErr)

	metadata := BashResponseMetadata{
		StartTime:        startTime.UnixMilli(),
		EndTime:          time.Now().UnixMilli(),
		Output:           stdout,
		Description:      params.Description,
		Background:       params.RunInBackground,
		WorkingDirectory: bgShell.WorkingDir,
	}
	if stdout == "" {
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
	}
	stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(bgShell.WorkingDir))
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
}

// stillRunningBashResponse builds the response for a shell that outlived
// its wait window and is being left running as a background job. message
// is a Printf format taking bgShell.ID, so the two call sites can each keep
// their own wording for why the command ended up backgrounded.
func stillRunningBashResponse(bgShell *shell.BackgroundShell, params BashParams, startTime time.Time, message string) fantasy.ToolResponse {
	metadata := BashResponseMetadata{
		StartTime:        startTime.UnixMilli(),
		EndTime:          time.Now().UnixMilli(),
		Description:      params.Description,
		WorkingDirectory: bgShell.WorkingDir,
		Background:       true,
		ShellID:          bgShell.ID,
	}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(fmt.Sprintf(message, bgShell.ID)), metadata)
}

// formatOutput formats the output of a completed command with error handling
func formatOutput(stdout, stderr string, execErr error) string {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)

	stdout = truncateOutput(stdout)
	stderr = truncateOutput(stderr)

	errorMessage := stderr
	if errorMessage == "" && execErr != nil {
		errorMessage = execErr.Error()
	}

	if interrupted {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += "Command was aborted before completion"
	} else if exitCode != 0 {
		if errorMessage != "" {
			errorMessage += "\n"
		}
		errorMessage += fmt.Sprintf("Exit code %d", exitCode)
	}

	hasBothOutputs := stdout != "" && stderr != ""

	if hasBothOutputs {
		stdout += "\n"
	}

	if errorMessage != "" {
		stdout += "\n" + errorMessage
	}

	return stdout
}

func TruncateOutput(content string) string {
	if ansi.StringWidth(content) <= MaxOutputLength {
		return content
	}

	halfLength := MaxOutputLength / 2
	start := ansi.Truncate(content, halfLength, "")
	end := ansi.TruncateLeft(content, ansi.StringWidth(content)-halfLength, "")

	truncatedLinesCount := max(strings.Count(content, "\n")-strings.Count(start, "\n")-strings.Count(end, "\n"), 0)
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...\n\n%s", start, truncatedLinesCount, end)
}

func truncateOutput(content string) string {
	return TruncateOutput(content)
}

func normalizeWorkingDir(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ReplaceAll(path, fsext.WindowsWorkingDirDrive(), "")
	}
	return filepath.ToSlash(path)
}

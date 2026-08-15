package proto

// The wire schema for per-tool permission parameters is owned by the
// tool itself, not duplicated here. We alias the canonical types so
// there is exactly one source of truth and so values survive a
// round-trip across the client/server boundary as the same Go type
// the UI asserts on.
import "github.com/rave-soft/braid/internal/agent/tools"

const BashToolName = "bash"

// BashParams represents the parameters for the bash tool.
type BashParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir,omitempty"`
	RunInBackground     bool   `json:"run_in_background,omitempty"`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty"`
}

// BashPermissionsParams represents the permission parameters for the bash tool.
type BashPermissionsParams = tools.BashPermissionsParams

// BashResponseMetadata represents the metadata for a bash tool response.
type BashResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
	Background       bool   `json:"background,omitempty"`
	ShellID          string `json:"shell_id,omitempty"`
}

// DiagnosticsParams represents the parameters for the diagnostics tool.
type DiagnosticsParams struct {
	FilePath string `json:"file_path"`
}

const DownloadToolName = "download"

// DownloadParams represents the parameters for the download tool.
type DownloadParams struct {
	URL      string `json:"url"`
	FilePath string `json:"file_path"`
	Timeout  int    `json:"timeout,omitempty"`
}

// DownloadPermissionsParams represents the permission parameters for the download tool.
type DownloadPermissionsParams = tools.DownloadPermissionsParams

const EditToolName = "edit"

// EditParams represents the parameters for the edit tool.
type EditParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditPermissionsParams represents the permission parameters for the edit tool.
type EditPermissionsParams = tools.EditPermissionsParams

// EditResponseMetadata represents the metadata for an edit tool response.
type EditResponseMetadata struct {
	Additions  int    `json:"additions"`
	Removals   int    `json:"removals"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

const FetchToolName = "fetch"

// FetchParams represents the parameters for the fetch tool.
type FetchParams struct {
	URL     string `json:"url"`
	Format  string `json:"format"`
	Timeout int    `json:"timeout,omitempty"`
}

// FetchPermissionsParams represents the permission parameters for the fetch tool.
type FetchPermissionsParams = tools.FetchPermissionsParams

// AgenticFetchToolName is the name of the agentic_fetch tool.
const AgenticFetchToolName = tools.AgenticFetchToolName

// AgenticFetchPermissionsParams represents the permission parameters for the
// agentic_fetch tool.
type AgenticFetchPermissionsParams = tools.AgenticFetchPermissionsParams

const GlobToolName = "glob"

// GlobParams represents the parameters for the glob tool.
type GlobParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// GlobResponseMetadata represents the metadata for a glob tool response.
type GlobResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

const GrepToolName = "grep"

// GrepResponseMetadata represents the metadata for a grep tool response.
type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	Truncated       bool `json:"truncated"`
}

const RipgrepToolName = "ripgrep"

// RipgrepParams represents the parameters for the ripgrep tool.
type RipgrepParams struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Include         string `json:"include"`
	LiteralText     bool   `json:"literal_text"`
	CaseInsensitive bool   `json:"case_insensitive"`
}

const LSToolName = "ls"

// LSParams represents the parameters for the ls tool.
type LSParams struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
}

// LSPermissionsParams represents the permission parameters for the ls tool.
type LSPermissionsParams = tools.LSPermissionsParams

// LSResponseMetadata represents the metadata for an ls tool response.
type LSResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

const MultiEditToolName = "multiedit"

// MultiEditOperation represents a single edit operation in a multi-edit.
type MultiEditOperation struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// MultiEditParams represents the parameters for the multi-edit tool.
type MultiEditParams struct {
	FilePath string               `json:"file_path"`
	Edits    []MultiEditOperation `json:"edits"`
}

// MultiEditPermissionsParams represents the permission parameters for the multi-edit tool.
type MultiEditPermissionsParams = tools.MultiEditPermissionsParams

// MultiEditResponseMetadata represents the metadata for a multi-edit tool response.
type FailedEdit struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

type MultiEditResponseMetadata struct {
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
}

const (
	ReadToolName = tools.ReadToolName
	// LegacyReadToolName is the pre-rename name of the read tool, still
	// present in sessions recorded before the rename. See
	// [tools.LegacyReadToolName].
	LegacyReadToolName = tools.LegacyReadToolName
)

// ReadParams represents the parameters for the read tool.
type ReadParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// ReadPermissionsParams represents the permission parameters for the read tool.
type ReadPermissionsParams = tools.ReadPermissionsParams

// ReadResponseMetadata represents the metadata for a read tool response.
type ReadResourceType string

const ReadResourceSkill ReadResourceType = "skill"

type ReadResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	ResourceType        ReadResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
}

const WriteToolName = "write"

// WriteParams represents the parameters for the write tool.
type WriteParams struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// WritePermissionsParams represents the permission parameters for the write tool.
type WritePermissionsParams = tools.WritePermissionsParams

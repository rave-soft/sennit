package proto

// Per-tool permission parameter types are defined here, in the leaf
// package, and aliased from internal/agent/tools. That direction (rather
// than tools defining them and proto aliasing back) keeps proto free of
// the tools package's heavy dependency graph while preserving type
// identity: the permission dialog holds a value the agent constructed as
// tools.*PermissionsParams and asserts on proto.*PermissionsParams, which
// only works if both names denote the same Go type.

const BashToolName = "bash"

const (
	GitStatusToolName = "git_status"
	GitDiffToolName   = "git_diff"
	GitLogToolName    = "git_log"
)

// BashParams represents the parameters for the bash tool.
type BashParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir,omitempty"`
	RunInBackground     bool   `json:"run_in_background,omitempty"`
	AutoBackgroundAfter int    `json:"auto_background_after,omitempty"`
}

// BashPermissionsParams represents the permission parameters for the bash tool.
type BashPermissionsParams struct {
	Description         string `json:"description"`
	Command             string `json:"command"`
	WorkingDir          string `json:"working_dir"`
	RunInBackground     bool   `json:"run_in_background"`
	AutoBackgroundAfter int    `json:"auto_background_after"`
}

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
type DownloadPermissionsParams struct {
	URL      string `json:"url"`
	FilePath string `json:"file_path"`
	Timeout  int    `json:"timeout,omitempty"`
}

const EditToolName = "edit"

// EditParams represents the parameters for the edit tool.
type EditParams struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditPermissionsParams represents the permission parameters for the edit tool.
type EditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

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
type FetchPermissionsParams struct {
	URL     string `json:"url"`
	Format  string `json:"format"`
	Timeout int    `json:"timeout,omitempty"`
}

// AgenticFetchToolName is the name of the agentic_fetch tool.
const AgenticFetchToolName = "agentic_fetch"

// AgenticFetchPermissionsParams represents the permission parameters for the
// agentic_fetch tool.
type AgenticFetchPermissionsParams struct {
	URL    string `json:"url,omitempty"`
	Prompt string `json:"prompt"`
}

const GlobToolName = "glob"

// GlobParams represents the parameters for the glob tool.
//
// It is the canonical data-only definition and is aliased by
// internal/agent/tools so the UI and runtime cannot drift.
type GlobParams struct {
	Pattern    string `json:"pattern" description:"The glob pattern to match files against"`
	Path       string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	MaxResults int    `json:"max_results,omitempty" description:"Maximum results (1-1000, defaults to 100)"`
	Cursor     string `json:"cursor,omitempty" description:"Stable continuation token returned by a previous response"`
}

// GlobResponseMetadata represents the metadata for a glob tool response.
type GlobResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	TotalFiles    int  `json:"total_files"`
	Truncated     bool `json:"truncated"`
	// Incomplete is true when part of the search tree could not be read
	// and was silently left out of the match set. It is reported
	// separately from Truncated, which means the result limit cut the
	// matches short — a different fact from part of the tree never
	// having been read at all. See tools.GlobResponseMetadata, the
	// agent-side twin of this DTO.
	Incomplete bool `json:"incomplete,omitempty"`
}

// GlobPermissionsParams represents the permission parameters for the glob tool.
type GlobPermissionsParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Cursor     string `json:"cursor"`
}

const GrepToolName = "grep"

// GrepResponseMetadata represents the metadata for a grep tool response.
type GrepResponseMetadata struct {
	NumberOfMatches int  `json:"number_of_matches"`
	TotalMatches    int  `json:"total_matches"`
	Truncated       bool `json:"truncated"`
	// Incomplete is true when part of the search tree could not be walked
	// and was silently left out of the match set. It is reported
	// separately from Truncated, which means the result limit cut the
	// matches short — a different fact from part of the tree never
	// having been searched at all. See tools.GrepResponseMetadata, the
	// agent-side twin of this DTO.
	Incomplete bool `json:"incomplete,omitempty"`
}

// GrepPermissionsParams represents the permission parameters for the grep
// tool. GrepParams itself is not aliased into proto (unlike glob/ripgrep),
// but the permission dialog still asserts on the proto type, so this one
// needs the same alias treatment as the others.
type GrepPermissionsParams struct {
	Pattern       string `json:"pattern"`
	Path          string `json:"path"`
	Include       string `json:"include"`
	LiteralText   bool   `json:"literal_text"`
	MaxResults    int    `json:"max_results"`
	BeforeContext int    `json:"before_context"`
	AfterContext  int    `json:"after_context"`
	Cursor        string `json:"cursor"`
	Sort          string `json:"sort"`
}

const RipgrepToolName = "ripgrep"

// RipgrepParams represents the parameters for the ripgrep tool.
//
// It is the canonical data-only definition and is aliased by
// internal/agent/tools so the UI and runtime cannot drift.
type RipgrepParams struct {
	Pattern         string `json:"pattern" description:"The regex pattern (Rust regex syntax) to search for in file contents"`
	Path            string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
	Include         string `json:"include,omitempty" description:"Glob pattern for files to include in the search (e.g. \"*.js\", \"*.{ts,tsx}\")"`
	LiteralText     bool   `json:"literal_text,omitempty" description:"If true, the pattern will be treated as literal text with special regex characters escaped. Default is false."`
	CaseInsensitive bool   `json:"case_insensitive,omitempty" description:"If true, the search is case-insensitive. Default is false."`
	MaxResults      int    `json:"max_results,omitempty" description:"Maximum results (1-1000, defaults to 100)"`
	BeforeContext   int    `json:"before_context,omitempty" description:"Lines before each match (0-30)"`
	AfterContext    int    `json:"after_context,omitempty" description:"Lines after each match (0-30)"`
	Cursor          string `json:"cursor,omitempty" description:"Stable continuation token"`
	Sort            string `json:"sort,omitempty" description:"Sort by path or mtime" enum:"path,mtime"`
}

// RipgrepPermissionsParams represents the permission parameters for the
// ripgrep tool.
type RipgrepPermissionsParams struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Include         string `json:"include"`
	LiteralText     bool   `json:"literal_text"`
	CaseInsensitive bool   `json:"case_insensitive"`
	MaxResults      int    `json:"max_results"`
	BeforeContext   int    `json:"before_context"`
	AfterContext    int    `json:"after_context"`
	Cursor          string `json:"cursor"`
	Sort            string `json:"sort"`
}

const LSToolName = "ls"

// LSParams represents the parameters for the ls tool.
type LSParams struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
}

// LSPermissionsParams represents the permission parameters for the ls tool.
type LSPermissionsParams struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
	Depth  int      `json:"depth"`
	Cursor string   `json:"cursor"`
}

// LSResponseMetadata represents the metadata for an ls tool response.
type LSResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	TotalFiles    int  `json:"total_files"`
	Truncated     bool `json:"truncated"`
	// Incomplete is true when part of the tree could not be read and was
	// silently left out of the results. It is reported separately from
	// Truncated, which means the result limit or depth cut the listing
	// short — a different fact from part of the tree never having been
	// read at all. See tools.LSResponseMetadata, the agent-side twin of
	// this DTO.
	Incomplete bool `json:"incomplete,omitempty"`
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
type MultiEditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

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
	ReadToolName = "read"
	// LegacyReadToolName is the pre-rename name of the read tool, still
	// present in sessions recorded before the rename. History renderers
	// and the config loader both keep accepting it.
	LegacyReadToolName = "view"
)

// ReadParams represents the parameters for the read tool.
type ReadParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// ReadPermissionsParams represents the permission parameters for the read tool.
type ReadPermissionsParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	Cursor   string `json:"cursor"`
}

// ReadResponseMetadata represents the metadata for a read tool response.
type ReadResourceType string

const ReadResourceSkill ReadResourceType = "skill"

type ReadResponseMetadata struct {
	FilePath   string `json:"file_path"`
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	NextOffset int    `json:"next_offset,omitempty"`
	// Truncated means the read stopped at the line limit before reaching
	// TotalLines; NextOffset says where to resume. See
	// tools.ReadResponseMetadata, the agent-side twin of this DTO.
	Truncated           bool             `json:"truncated"`
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
type WritePermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

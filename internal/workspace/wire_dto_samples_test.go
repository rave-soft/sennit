package workspace

import (
	"reflect"
	"time"

	"charm.land/catwalk/pkg/catwalk"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/csync"
	"github.com/rave-soft/sennit/internal/git"
	"github.com/rave-soft/sennit/internal/history"
	"github.com/rave-soft/sennit/internal/hooks"
	"github.com/rave-soft/sennit/internal/message"
	"github.com/rave-soft/sennit/internal/oauth"
	"github.com/rave-soft/sennit/internal/permission"
	"github.com/rave-soft/sennit/internal/proto"
	"github.com/rave-soft/sennit/internal/providers/accounts"
	providerconfig "github.com/rave-soft/sennit/internal/providers/config"
	providerstate "github.com/rave-soft/sennit/internal/providers/state"
	"github.com/rave-soft/sennit/internal/question"
	"github.com/rave-soft/sennit/internal/session"
	"github.com/rave-soft/sennit/internal/skills"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/wireerr"
)

func ptr[T any](v T) *T { return &v }

// sampleTime is a fixed, non-zero, monotonic-clock-free UTC time. Every
// plain time.Time sample in this file reuses it (its own field, or a
// RFC-3339-codec type's field): time.Now() carries a monotonic reading
// that json round-trips away (encoding/json has no wire representation
// for it), which would make require.Equal fail the round trip for a
// reason that has nothing to do with the type's wire safety.
// time.Date(...).UTC() has no monotonic component to begin with.
//
// accounts.Usage is the one exception to "UTC" - see sampleUsageTime
// below, next to accounts.Usage's own sample, for why.
var sampleTime = time.Date(2026, 3, 4, 15, 6, 7, 0, time.UTC)

// -- wireSampleZeroExemptions ------------------------------------------

var wireSampleZeroExemptions = map[string]string{
	// json:"-": never crosses the wire, so a real value would prove
	// nothing about JSON fidelity - and would actually break the round
	// trip, since Marshal drops it and Unmarshal always produces zero.
	"github.com/rave-soft/sennit/internal/config.Config.RuntimeProviders": `json:"-"`,
	"github.com/rave-soft/sennit/internal/config.Config.Problems":         `json:"-"`,
	// json:"-": never travels; see skills.Skill's own doc comment on why
	// the field exists at all (it is for handing a skill to another
	// in-process workspace, not for the wire).
	"github.com/rave-soft/sennit/internal/skills.Skill.Source": `yaml:"-" json:"-"`,
	// A notification is either a grant or a denial, never both - Denied's
	// zero value (false) is the correct value alongside Granted: true, not
	// a forgotten field.
	"github.com/rave-soft/sennit/internal/permission.PermissionNotification.Denied": "mutually exclusive with Granted: true in this sample",
}

// -- shared nested-type samples ------------------------------------------

var sampleOAuthClient = oauth.OAuthClient{
	ClientID:     "client-id",
	ClientSecret: "client-secret",
	AuthURL:      "https://example.com/authorize",
	TokenURL:     "https://example.com/token",
	AuthStyle:    1,
}

var sampleOAuthToken = oauth.Token{
	AccessToken:  "access-token",
	RefreshToken: "refresh-token",
	ExpiresIn:    3600,
	ExpiresAt:    sampleTime.Unix(),
	Client:       &sampleOAuthClient,
}

var sampleCatwalkModelOptions = catwalk.ModelOptions{
	Temperature:      ptr(0.5),
	TopP:             ptr(0.9),
	TopK:             ptr(int64(40)),
	FrequencyPenalty: ptr(0.1),
	PresencePenalty:  ptr(0.2),
	ProviderOptions:  map[string]any{"reasoning": "high"},
}

var sampleCatwalkModel = catwalk.Model{
	ID:                     "gpt-5",
	Name:                   "GPT-5",
	CostPer1MIn:            1,
	CostPer1MOut:           2,
	CostPer1MInCached:      0.5,
	CostPer1MOutCached:     0.25,
	ContextWindow:          200000,
	DefaultMaxTokens:       4096,
	CanReason:              true,
	ReasoningLevels:        []string{"low", "high"},
	DefaultReasoningEffort: "medium",
	SupportsImages:         true,
	Options:                sampleCatwalkModelOptions,
}

var sampleCatwalkProvider = catwalk.Provider{
	Name:                "OpenAI",
	ID:                  catwalk.InferenceProviderOpenAI,
	APIKey:              "$OPENAI_API_KEY",
	APIEndpoint:         "https://api.openai.com/v1",
	Type:                catwalk.TypeOpenAI,
	DefaultLargeModelID: "gpt-5",
	DefaultSmallModelID: "gpt-5-mini",
	Models:              []catwalk.Model{sampleCatwalkModel},
	DefaultHeaders:      map[string]string{"X-Extra": "1"},
}

var sampleSelectedModel = config.SelectedModel{
	Model:            "gpt-5",
	Provider:         "openai",
	ReasoningEffort:  "high",
	Think:            true,
	MaxTokens:        4096,
	Temperature:      ptr(0.5),
	TopP:             ptr(0.9),
	TopK:             ptr(int64(40)),
	FrequencyPenalty: ptr(0.1),
	PresencePenalty:  ptr(0.2),
	ProviderOptions:  map[string]any{"reasoning": "high"},
}

var sampleProviderConfig = providerconfig.ProviderConfig{
	ID:                 "openai",
	Name:               "OpenAI",
	BaseURL:            "https://api.openai.com/v1",
	ProxyURL:           "http://localhost:8080",
	Type:               catwalk.TypeOpenAI,
	APIKey:             "$OPENAI_API_KEY",
	OAuthToken:         &sampleOAuthToken,
	Disable:            false,
	Account:            "acct-1",
	SystemPromptPrefix: "You are helpful.",
	ExtraHeaders:       map[string]string{"X-Extra": "1"},
}

var sampleProviderStateProvider = providerstate.Provider{
	ID:                 "openai",
	Name:               "OpenAI",
	BaseURL:            "https://api.openai.com/v1",
	Type:               catwalk.TypeOpenAI,
	APIKey:             "$OPENAI_API_KEY",
	APIKeyTemplate:     "$OPENAI_API_KEY",
	OAuthToken:         &sampleOAuthToken,
	ProxyURL:           "http://localhost:8080",
	ConfiguredProxyURL: "http://localhost:8080",
	Account:            "acct-1",
	ExtraHeaders:       map[string]string{"X-Extra": "1"},
	ExtraParams:        map[string]string{"foo": "bar"},
	Models:             []catwalk.Model{sampleCatwalkModel},
}

var sampleProvidersMap = csync.NewMap(map[string]providerconfig.ProviderConfig{"openai": sampleProviderConfig})

var sampleRuntimeProvidersMap = csync.NewMap(map[string]providerstate.Provider{"openai": sampleProviderStateProvider})

var sampleHook = hooks.Hook{
	Name:    "lint on write",
	Matcher: "edit|write",
	Command: "task lint",
	Timeout: 30,
}

var sampleCompletions = config.Completions{
	MaxDepth: ptr(10),
	MaxItems: ptr(100),
}

var sampleTUIOptions = config.TUIOptions{
	CompactMode: true,
	DiffMode:    "split",
	Theme:       "steel-teal",
	Completions: sampleCompletions,
	Transparent: ptr(true),
	Scrollbar:   config.ScrollbarAlways,
	Spinner:     config.SpinnerPulse,
	Keybindings: map[string][]string{"quit": {"ctrl+c"}},
}

var sampleAttribution = config.Attribution{
	TrailerStyle:  config.TrailerStyleAssistedBy,
	CoAuthoredBy:  ptr(true),
	GeneratedWith: true,
}

var sampleAutoSummarizeIdle = config.AutoSummarizeIdleOptions{
	Enabled:       ptr(true),
	ContextTokens: 60000,
	After:         "4m",
}

var sampleWebSearchOptions = config.WebSearchOptions{
	Provider: "tavily",
	APIKey:   "$TAVILY_API_KEY",
	BaseURL:  "https://api.tavily.com/search",
	ProxyURL: "http://localhost:8080",
}

var sampleThreadsOptions = config.ThreadsOptions{
	WorktreeDir: "/var/tmp/sennit-threads",
}

var sampleOptions = config.Options{
	ContextPaths:            []string{"CLAUDE.md"},
	GlobalContextPaths:      []string{"~/.config/AGENTS.md"},
	SkillsPaths:             []string{"./skills"},
	TUI:                     &sampleTUIOptions,
	Debug:                   true,
	DebugLSP:                true,
	DisableAutoSummarize:    true,
	AutoSummarizeAt:         100000,
	AutoSummarizeIdle:       &sampleAutoSummarizeIdle,
	DataDirectory:           ".sennit",
	DisabledTools:           []string{"bash"},
	DisableDefaultProviders: true,
	Attribution:             &sampleAttribution,
	DisableMetrics:          true,
	InitializeAs:            "AGENTS.md",
	AutoLSP:                 ptr(true),
	Progress:                ptr(true),
	Notifications:           "auto",
	DisabledSkills:          []string{"sennit-config"},
	WebSearch:               &sampleWebSearchOptions,
	Threads:                 &sampleThreadsOptions,
	BackgroundAgents:        ptr(true),
	HistoryRetentionDays:    ptr(90),
}

var samplePermissions = config.Permissions{
	AllowedTools: []string{"bash", "read"},
	Bypass:       true,
}

var sampleToolLs = config.ToolLs{
	MaxDepth: ptr(10),
	MaxItems: ptr(100),
}

var sampleToolGrep = config.ToolGrep{
	Timeout: ptr(5 * time.Second),
}

var sampleToolGlob = config.ToolGlob{
	Timeout: ptr(30 * time.Second),
}

var sampleTools = config.Tools{
	Ls:   sampleToolLs,
	Grep: sampleToolGrep,
	Glob: sampleToolGlob,
}

var sampleAgent = config.Agent{
	ID:              "reviewer",
	Name:            "Reviewer",
	Description:     "Reviews diffs",
	Disabled:        true,
	Model:           "openai/gpt-5",
	Prompt:          "You review code.",
	ReasoningEffort: "high",
	AllowedTools:    []string{"bash", "read"},
	AllowedMCP:      map[string][]string{"myserver": {"tool1"}},
	ContextPaths:    []string{"AGENTS.md"},
}

var sampleLSPConfig = config.LSPConfig{
	Disabled:    true,
	Command:     "gopls",
	Args:        []string{"serve"},
	Env:         map[string]string{"GOFLAGS": "-mod=mod"},
	FileTypes:   []string{"go"},
	RootMarkers: []string{"go.mod"},
	InitOptions: map[string]any{"usePlaceholders": true},
	Options:     map[string]any{"staticcheck": true},
	Timeout:     60,
}

var sampleMCPConfig = config.MCPConfig{
	Command:           "npx",
	Env:               map[string]string{"NODE_ENV": "production"},
	Args:              []string{"-y", "some-mcp-server"},
	Type:              config.MCPStdio,
	URL:               "http://localhost:3000/mcp",
	Disabled:          true,
	DisabledTools:     []string{"dangerous_tool"},
	EnabledTools:      []string{"safe_tool"},
	Timeout:           30,
	Headers:           map[string]string{"Authorization": "Bearer x"},
	OAuth:             true,
	OAuthClientID:     "client-id",
	OAuthClientSecret: "client-secret",
	OAuthCallbackPort: 51000,
	OAuthToken:        &sampleOAuthToken,
}

var sampleConfig = config.Config{
	Schema:       "https://sennit.dev/schema.json",
	Model:        sampleSelectedModel,
	RecentModels: []config.SelectedModel{sampleSelectedModel},
	Providers:    sampleProvidersMap,
	// RuntimeProviders: json:"-", left zero (see wireSampleZeroExemptions).
	MCP:         config.MCPs{"myserver": sampleMCPConfig},
	LSP:         config.LSPs{"gopls": sampleLSPConfig},
	Options:     &sampleOptions,
	Permissions: &samplePermissions,
	Tools:       sampleTools,
	Hooks:       map[string][]config.HookConfig{"PreToolUse": {sampleHook}},
	Env:         map[string]string{"FOO": "bar"},
	Agents:      map[string]config.Agent{"reviewer": sampleAgent},
	// Problems: json:"-", left zero (see wireSampleZeroExemptions).
}

var sampleWireErrQuota = wireerr.Quota{
	Model:       "gpt-5",
	SettingsURL: "https://example.com/settings",
}

var sampleWireErrReadOnly = wireerr.ReadOnly{
	Operation: "AgentRun",
	Reason:    "attached read-only",
}

var sampleWireErr = wireerr.Error{
	Code:     "read_only",
	Message:  "operation refused: read-only workspace",
	Quota:    &sampleWireErrQuota,
	ReadOnly: &sampleWireErrReadOnly,
}

// sampleUsageTime is accounts.Usage's own exception to the "time.Time
// samples are UTC" rule above: Usage's codec (MarshalJSON/UnmarshalJSON
// in internal/providers/accounts/accounts.go) writes Unix seconds and
// reads them back with time.Unix, which deliberately returns the
// decoding process's local Location - these times are formatted for a
// person with a clock layout (rotator.go's ErrAllExhausted.Error,
// internal/agent/provider_limit.go, both ResetsAt.Format("15:04")), and
// on a remote client the decoding process's local zone is the person's
// own. Building the sample the same way (time.Unix, not time.Date(...).UTC())
// matches what a real decode produces, so require.Equal on other fields
// that embed a Usage is comparing like with like; Usage itself is opaque
// (own MarshalJSON/UnmarshalJSON) so its own round trip is checked by
// re-encoding to JSON, which only needs the instant to match. Second
// precision: that is what the wire format carries.
var sampleUsageTime = time.Unix(sampleTime.Unix(), 0)

var sampleAccountUsage = accounts.Usage{
	Plan: "pro",
	Primary: accounts.UsageWindow{
		UsedPercent:   42,
		WindowMinutes: 300,
		ResetsAt:      sampleUsageTime,
	},
	Secondary: accounts.UsageWindow{
		UsedPercent:   10,
		WindowMinutes: 10080,
		ResetsAt:      sampleUsageTime,
	},
	CapturedAt: sampleUsageTime,
}

var sampleAccount = accounts.Account{
	ID:        "acct-1",
	Label:     "Work",
	AccountID: "acct-remote-1",
	Email:     "person@example.com",
	ProxyURL:  "http://localhost:8080",
	Token:     &sampleOAuthToken,
	APIKey:    "$OPENAI_API_KEY",
	Disabled:  true,
	Usage:     sampleAccountUsage,
}

var sampleLegacyCredential = accounts.LegacyCredential{
	APIKey:          "$OPENAI_API_KEY",
	Token:           &sampleOAuthToken,
	ProxyURL:        "http://localhost:8080",
	AccountID:       "acct-remote-1",
	Email:           "person@example.com",
	Label:           "Work",
	ForceNewAccount: true,
}

var sampleGitFileChange = git.FileChange{
	Path:      "internal/foo.go",
	Additions: 3,
	Deletions: 1,
}

var sampleHistoryFile = history.File{
	ID:        "hist-1",
	SessionID: "sess-1",
	Path:      "internal/foo.go",
	Content:   "package foo",
	Version:   2,
	CreatedAt: sampleTime.Unix(),
	UpdatedAt: sampleTime.Unix(),
}

var sampleMessage = message.Message{
	ID:        "msg-1",
	Role:      message.Assistant,
	SessionID: "sess-1",
	Parts:     []message.ContentPart{message.TextContent{Text: "hi"}},
	Model:     "gpt-5",
	Provider:  "openai",
	CreatedAt: sampleTime.Unix(),
	UpdatedAt: sampleTime.Unix(),
	// IsSummaryMessage/Origin/SummaryBeforeTokens/SummaryAfterTokens
	// deliberately left at their zero value: Message is opaque (its own
	// MarshalJSON/UnmarshalJSON) so the per-field zero guard never runs on
	// it - only the round trip does, and it does not need every field
	// populated to prove that.
}

var sampleMessageAttachment = message.Attachment{
	FilePath: "/tmp/file.txt",
	FileName: "file.txt",
	MimeType: "text/plain",
	Content:  []byte("hello"),
}

// samplePermissionRequest.Params is a proto.BashPermissionsParams value,
// not a bare map: PermissionRequest.UnmarshalJSON decodes Params through
// proto.DecodePermissionParams keyed on ToolName ("bash" here), which
// always reconstructs the tool's own typed Params struct - so the sample
// has to already hold what that decode produces for the round trip to
// come back equal. See DecodePermissionParams' doc comment.
var samplePermissionRequest = permission.PermissionRequest{
	ID:          "perm-1",
	SessionID:   "sess-1",
	ToolCallID:  "call-1",
	ToolName:    proto.BashToolName,
	Description: "run a command",
	Action:      "execute",
	Params: proto.BashPermissionsParams{
		Description:         "run a command",
		Command:             "echo hi",
		WorkingDir:          "/repo",
		RunInBackground:     true,
		AutoBackgroundAfter: 30,
	},
	Path: "/repo",
	// Delegation is `json:"-"`; PermissionRequest is opaque so the
	// per-field zero guard does not run on it.
}

var samplePermissionNotification = permission.PermissionNotification{
	ToolCallID: "call-1",
	Granted:    true,
	// Denied is left false (its zero value): a notification is either a
	// grant or a denial, never both, so there is no non-zero value to put
	// here that would not misrepresent the sample. Exempted below.
}

var sampleProtoThread = proto.Thread{
	ID:              "thread-1",
	Name:            "fix-bug",
	Goal:            "fix the bug",
	BaseBranch:      "main",
	Branch:          "sennit/fix-bug",
	WorktreePath:    "/repo/.sennit/threads/fix-bug",
	WorkspaceID:     "ws-1",
	SessionID:       "sess-1",
	Status:          "failed",
	Kind:            "thread",
	ResultSummary:   "fixed it",
	Error:           "exit status 1",
	CreatedAt:       sampleTime.Unix(),
	UpdatedAt:       sampleTime.Unix(),
	CompletedAt:     sampleTime.Unix(),
	ParentSessionID: "parent-sess-1",
}

var sampleProtoCreateThreadRequest = proto.CreateThreadRequest{
	Name:            "fix-bug",
	Goal:            "fix the bug",
	BaseBranch:      "main",
	ParentSessionID: "parent-sess-1",
}

var sampleProtoRemoveThreadOptions = proto.RemoveThreadOptions{
	Force:        true,
	DeleteBranch: true,
}

var sampleProtoLSPClientInfo = proto.LSPClientInfo{
	Name:            "gopls",
	State:           proto.LSPStateError,
	Error:           "connection refused",
	DiagnosticCount: 3,
	ConnectedAt:     sampleTime,
}

var sampleProtoLSPDiagnosticCounts = proto.LSPDiagnosticCounts{
	Error:       1,
	Warning:     2,
	Information: 3,
	Hint:        4,
}

var sampleProtoShellCommandResponse = proto.ShellCommandResponse{
	Output:   "hello\n",
	ExitCode: 1,
	Canceled: true,
}

var sampleQuestionChoice = question.Choice{
	ID:          "choice-1",
	Label:       "Yes",
	Description: "confirm",
}

var sampleQuestionQuestion = question.Question{
	ID:          "q-1",
	Type:        question.TypeSingleChoice,
	Label:       "Proceed?",
	Text:        "Should I proceed?",
	Description: "Confirm before continuing",
	Choices:     []question.Choice{sampleQuestionChoice},
}

var sampleQuestionRequest = question.Request{
	ID:                 "batch-1",
	SessionID:          "sess-1",
	ToolCallID:         "call-1",
	Questions:          []question.Question{sampleQuestionQuestion},
	ConfirmTitle:       "Ready to go?",
	ConfirmDescription: "Review your answers",
}

var sampleQuestionAnswer = question.Answer{
	QuestionID:  "q-1",
	SelectedIDs: []string{"choice-1"},
	FillInText:  "yes",
	Yes:         ptr(true),
	Notes:       map[string]string{"note": "ok"},
}

var sampleQuestionNotification = question.Notification{
	BatchID: "batch-1",
}

var sampleSessionModelRef = session.ModelRef{
	Provider: "openai",
	Model:    "gpt-5",
}

var sampleSessionTodo = session.Todo{
	Content:    "write tests",
	Status:     session.TodoStatusInProgress,
	ActiveForm: "Writing tests",
}

var sampleSession = session.Session{
	ID:               "sess-1",
	ParentSessionID:  "parent-sess-1",
	Model:            sampleSessionModelRef,
	AgentID:          "reviewer",
	Title:            "Fix the bug",
	MessageCount:     5,
	PromptTokens:     100,
	CompletionTokens: 50,
	EstimatedUsage:   true,
	SummaryMessageID: "msg-summary-1",
	Cost:             0.42,
	Todos:            []session.Todo{sampleSessionTodo},
	CreatedAt:        sampleTime.Unix(),
	UpdatedAt:        sampleTime.Unix(),
}

var sampleSkillsCatalogEntry = skills.CatalogEntry{
	ID:            "skill-1",
	Name:          "reviewer",
	Description:   "reviews code",
	Label:         "user",
	Source:        skills.SourceUser,
	UserInvocable: true,
}

var sampleSkillsSkillReadResult = skills.SkillReadResult{
	Name:        "reviewer",
	Description: "reviews code",
	Source:      skills.SourceUser,
	Builtin:     true,
}

var sampleSkillsSkill = skills.Skill{
	Name:                      "reviewer",
	Description:               "reviews code",
	UserInvocable:             true,
	DisableModelInvocation:    true,
	DisableSubagentInvocation: true,
	License:                   "MIT",
	Compatibility:             "sennit>=1.0",
	Metadata:                  map[string]string{"owner": "team-x"},
	Instructions:              "do the review",
	Path:                      "/repo/.sennit/skills/reviewer",
	SkillFilePath:             "/repo/.sennit/skills/reviewer/SKILL.md",
	Builtin:                   true,
	// Source: json:"-", left zero (see wireSampleZeroExemptions).
}

var sampleSkillsSkillState = skills.SkillState{
	Name:  "reviewer",
	Path:  "/repo/.sennit/skills/reviewer",
	State: skills.StateError,
	Err:   &sampleWireErr,
}

var sampleSkillsEvent = skills.Event{
	States: []*skills.SkillState{&sampleSkillsSkillState},
}

var sampleStatsRequest = stats.Request{
	Scope:       stats.ScopeProject,
	SessionID:   "sess-1",
	ProjectPath: "/repo",
	Since:       sampleTime.Unix(),
	WithSkills:  true,
	WithLatency: true,
}

var sampleStatsModel = stats.Model{
	Model:            "gpt-5",
	Provider:         "openai",
	MessageCount:     10,
	TimeSeconds:      120,
	PromptTokens:     1000,
	CompletionTokens: 500,
	Cost:             1.23,
	Approximate:      true,
	Delegations:      2,
	Succeeded:        1,
}

var sampleStatsAgent = stats.Agent{
	Name:             "reviewer",
	Runs:             3,
	PromptTokens:     100,
	CompletionTokens: 50,
	Cost:             0.5,
	TimeSeconds:      30,
	Delegations:      1,
	Succeeded:        1,
}

var sampleStatsProject = stats.Project{
	Path:             "/repo",
	Sessions:         4,
	PromptTokens:     2000,
	CompletionTokens: 1000,
	Cost:             2.5,
	TimeSeconds:      300,
}

var sampleStatsSkill = stats.Skill{
	Name:         "reviewer",
	LoadCount:    5,
	SessionCount: 3,
	FirstUsedAt:  "2026-01-01T00:00:00Z",
	LastUsedAt:   "2026-03-04T00:00:00Z",
}

var sampleStatsLatency = stats.Latency{
	Kind:   "permission_wait",
	Events: 10,
	P50MS:  100,
	P95MS:  500,
	MaxMS:  900,
}

var sampleStatsOutcome = stats.Outcome{
	Total:  10,
	Landed: 8,
	Failed: 2,
}

var sampleStatsSnapshot = stats.Snapshot{
	Totals:   sampleStatsProject,
	Models:   []stats.Model{sampleStatsModel},
	Agents:   []stats.Agent{sampleStatsAgent},
	Projects: []stats.Project{sampleStatsProject},
	Skills:   []stats.Skill{sampleStatsSkill},
	Latency:  []stats.Latency{sampleStatsLatency},
	Outcome:  sampleStatsOutcome,
}

var sampleAccountCapabilities = AccountCapabilities{
	Usage:    true,
	RotateOn: RotateBoth,
	OAuth:    true,
}

var sampleAgentCatalog = AgentCatalog{
	ID:              "gpt-5",
	Name:            "GPT-5",
	CanReason:       true,
	ReasoningLevels: []string{"low", "high"},
	ContextWindow:   200000,
}

var sampleAgentSelection = AgentSelection{
	Provider:        "openai",
	Model:           "gpt-5",
	Think:           true,
	ReasoningEffort: "high",
}

var sampleAgentModel = AgentModel{
	CatalogCfg: sampleAgentCatalog,
	ModelCfg:   sampleAgentSelection,
}

var sampleAgentNotification = AgentNotification{
	SessionID:    "sess-1",
	SessionTitle: "Fix the bug",
	ChildSession: true,
	Type:         AgentNotificationFinished,
	ProviderID:   "openai",
	RunID:        "run-1",
	Message:      "done",
	AWSSOCommand: "aws sso login",
	AWSSOURL:     "https://example.com/sso",
}

var sampleAgentRunEvent = AgentRunEvent{
	TextDelta: "hello",
	Status:    "thinking",
	Done:      true,
	Err:       &sampleWireErr,
}

var sampleAgentRunOptions = AgentRunOptions{
	AutoApprovePermissions: true,
}

var sampleArgument = Argument{
	ID:          "arg-1",
	Title:       "Path",
	Description: "file path",
	Required:    true,
}

var sampleBackgroundJobCounts = BackgroundJobCounts{
	Active:    1,
	Completed: 2,
}

var sampleConfigureCustomProviderParams = ConfigureCustomProviderParams{
	ID:      "custom-1",
	Name:    "Custom",
	BaseURL: "https://api.custom.example/v1",
	Type:    "openai",
	APIKey:  "$CUSTOM_API_KEY",
}

var sampleCustomCommand = CustomCommand{
	ID:        "cmd-1",
	Name:      "review",
	Content:   "Review this diff.",
	Arguments: []Argument{sampleArgument},
	Skill:     &sampleSkillsSkill,
}

var sampleLSPEvent = LSPEvent{
	Type:            LSPEventStateChanged,
	Name:            "gopls",
	State:           proto.LSPStateReady,
	Error:           &sampleWireErr,
	DiagnosticCount: 3,
}

var sampleMCPCounts = MCPCounts{
	Tools:     3,
	Prompts:   2,
	Resources: 1,
}

var sampleMCPClientInfo = MCPClientInfo{
	Name:        "myserver",
	State:       MCPStateConnected,
	Error:       &sampleWireErr,
	Counts:      sampleMCPCounts,
	ConnectedAt: sampleTime,
}

var sampleMCPEvent = MCPEvent{
	Type: MCPEventStateChanged,
	Name: "myserver",
}

var sampleMCPPendingAuthServer = MCPPendingAuthServer{
	Name: "myserver",
	URL:  "https://example.com/authorize",
}

var sampleMCPPrompt = MCPPrompt{
	ID:          "prompt-1",
	Title:       "Summarize",
	Description: "Summarize the input",
	PromptID:    "prompt-1",
	ClientID:    "myserver",
	Arguments:   []Argument{sampleArgument},
}

var sampleMCPResourceContents = MCPResourceContents{
	URI:      "file:///repo/README.md",
	MIMEType: "text/markdown",
	Text:     "# Hello",
	Blob:     []byte("blob"),
}

var sampleMCPResourceInfo = MCPResourceInfo{
	MCPName:  "myserver",
	URI:      "file:///repo/README.md",
	Title:    "README",
	MIMEType: "text/markdown",
}

var sampleModelRefreshResult = ModelRefreshResult{
	ID:         "custom-1",
	Models:     10,
	Added:      2,
	Removed:    1,
	Updated:    3,
	Skipped:    true,
	SkipReason: "no base_url",
	Err:        &sampleWireErr,
}

var sampleOAuthCompletion = OAuthCompletion{
	Account:       sampleAccount,
	ModelsFetched: 5,
	ModelsError:   &sampleWireErr,
	ProxyError:    &sampleWireErr,
}

var sampleOAuthStartResult = OAuthStartResult{
	AuthorizationURL:       "https://example.com/authorize",
	DeviceCode:             "device-code",
	UserCode:               "USER-CODE",
	VerificationURL:        "https://example.com/verify",
	Interval:               5,
	ExpiresIn:              600,
	Token:                  &sampleOAuthToken,
	ReusedExistingLogin:    true,
	RefreshedExistingLogin: true,
	ExistingLoginFailure:   "previous login expired",
}

// wireSamples maps each type collectWireTypes finds to a fully populated
// sample value. See wire_dto_test.go's TestWireTypesRoundTripJSON for how
// this is checked (every exported field non-zero unless exempted above,
// then a json.Marshal/Unmarshal round trip).
var wireSamples = map[reflect.Type]any{
	reflectTypeOf[catwalk.Model]():        sampleCatwalkModel,
	reflectTypeOf[catwalk.ModelOptions](): sampleCatwalkModelOptions,
	reflectTypeOf[catwalk.Provider]():     sampleCatwalkProvider,

	reflectTypeOf[config.Agent]():                    sampleAgent,
	reflectTypeOf[config.Attribution]():              sampleAttribution,
	reflectTypeOf[config.AutoSummarizeIdleOptions](): sampleAutoSummarizeIdle,
	reflectTypeOf[config.Completions]():              sampleCompletions,
	reflectTypeOf[config.Config]():                   sampleConfig,
	reflectTypeOf[config.LSPConfig]():                sampleLSPConfig,
	reflectTypeOf[config.MCPConfig]():                sampleMCPConfig,
	reflectTypeOf[config.Options]():                  sampleOptions,
	reflectTypeOf[config.Permissions]():              samplePermissions,
	reflectTypeOf[config.Problem]():                  config.Problem{Severity: config.SeverityWarn, Area: config.AreaProvider, Subject: "openai", Message: "missing api key", Hint: "set OPENAI_API_KEY"},
	reflectTypeOf[config.SelectedModel]():            sampleSelectedModel,
	reflectTypeOf[config.TUIOptions]():               sampleTUIOptions,
	reflectTypeOf[config.ThreadsOptions]():           sampleThreadsOptions,
	reflectTypeOf[config.ToolGlob]():                 sampleToolGlob,
	reflectTypeOf[config.ToolGrep]():                 sampleToolGrep,
	reflectTypeOf[config.ToolLs]():                   sampleToolLs,
	reflectTypeOf[config.Tools]():                    sampleTools,
	reflectTypeOf[config.WebSearchOptions]():         sampleWebSearchOptions,

	reflectTypeOf[csync.Map[string, providerconfig.ProviderConfig]](): sampleProvidersMap,
	reflectTypeOf[csync.Map[string, providerstate.Provider]]():        sampleRuntimeProvidersMap,

	reflectTypeOf[git.FileChange](): sampleGitFileChange,

	reflectTypeOf[history.File](): sampleHistoryFile,

	reflectTypeOf[hooks.Hook](): sampleHook,

	reflectTypeOf[message.Attachment](): sampleMessageAttachment,
	reflectTypeOf[message.Message]():    sampleMessage,

	reflectTypeOf[oauth.OAuthClient](): sampleOAuthClient,
	reflectTypeOf[oauth.Token]():       sampleOAuthToken,

	reflectTypeOf[permission.PermissionNotification](): samplePermissionNotification,
	reflectTypeOf[permission.PermissionRequest]():      samplePermissionRequest,

	reflectTypeOf[proto.CreateThreadRequest]():  sampleProtoCreateThreadRequest,
	reflectTypeOf[proto.LSPClientInfo]():        sampleProtoLSPClientInfo,
	reflectTypeOf[proto.LSPDiagnosticCounts]():  sampleProtoLSPDiagnosticCounts,
	reflectTypeOf[proto.RemoveThreadOptions]():  sampleProtoRemoveThreadOptions,
	reflectTypeOf[proto.ShellCommandResponse](): sampleProtoShellCommandResponse,
	reflectTypeOf[proto.Thread]():               sampleProtoThread,

	reflectTypeOf[accounts.Account]():          sampleAccount,
	reflectTypeOf[accounts.LegacyCredential](): sampleLegacyCredential,
	reflectTypeOf[accounts.Usage]():            sampleAccountUsage,

	reflectTypeOf[question.Answer]():       sampleQuestionAnswer,
	reflectTypeOf[question.Choice]():       sampleQuestionChoice,
	reflectTypeOf[question.Notification](): sampleQuestionNotification,
	reflectTypeOf[question.Question]():     sampleQuestionQuestion,
	reflectTypeOf[question.Request]():      sampleQuestionRequest,

	reflectTypeOf[session.ModelRef](): sampleSessionModelRef,
	reflectTypeOf[session.Session]():  sampleSession,
	reflectTypeOf[session.Todo]():     sampleSessionTodo,

	reflectTypeOf[skills.CatalogEntry]():    sampleSkillsCatalogEntry,
	reflectTypeOf[skills.Event]():           sampleSkillsEvent,
	reflectTypeOf[skills.Skill]():           sampleSkillsSkill,
	reflectTypeOf[skills.SkillReadResult](): sampleSkillsSkillReadResult,
	reflectTypeOf[skills.SkillState]():      sampleSkillsSkillState,

	reflectTypeOf[stats.Agent]():    sampleStatsAgent,
	reflectTypeOf[stats.Latency]():  sampleStatsLatency,
	reflectTypeOf[stats.Model]():    sampleStatsModel,
	reflectTypeOf[stats.Outcome]():  sampleStatsOutcome,
	reflectTypeOf[stats.Project]():  sampleStatsProject,
	reflectTypeOf[stats.Request]():  sampleStatsRequest,
	reflectTypeOf[stats.Skill]():    sampleStatsSkill,
	reflectTypeOf[stats.Snapshot](): sampleStatsSnapshot,

	reflectTypeOf[wireerr.Error]():    sampleWireErr,
	reflectTypeOf[wireerr.Quota]():    sampleWireErrQuota,
	reflectTypeOf[wireerr.ReadOnly](): sampleWireErrReadOnly,

	reflectTypeOf[AccountCapabilities]():           sampleAccountCapabilities,
	reflectTypeOf[AgentCatalog]():                  sampleAgentCatalog,
	reflectTypeOf[AgentModel]():                    sampleAgentModel,
	reflectTypeOf[AgentNotification]():             sampleAgentNotification,
	reflectTypeOf[AgentRunEvent]():                 sampleAgentRunEvent,
	reflectTypeOf[AgentRunOptions]():               sampleAgentRunOptions,
	reflectTypeOf[AgentSelection]():                sampleAgentSelection,
	reflectTypeOf[Argument]():                      sampleArgument,
	reflectTypeOf[BackgroundJobCounts]():           sampleBackgroundJobCounts,
	reflectTypeOf[ConfigureCustomProviderParams](): sampleConfigureCustomProviderParams,
	reflectTypeOf[CustomCommand]():                 sampleCustomCommand,
	reflectTypeOf[LSPEvent]():                      sampleLSPEvent,
	reflectTypeOf[MCPClientInfo]():                 sampleMCPClientInfo,
	reflectTypeOf[MCPCounts]():                     sampleMCPCounts,
	reflectTypeOf[MCPEvent]():                      sampleMCPEvent,
	reflectTypeOf[MCPPendingAuthServer]():          sampleMCPPendingAuthServer,
	reflectTypeOf[MCPPrompt]():                     sampleMCPPrompt,
	reflectTypeOf[MCPResourceContents]():           sampleMCPResourceContents,
	reflectTypeOf[MCPResourceInfo]():               sampleMCPResourceInfo,
	reflectTypeOf[ModelRefreshResult]():            sampleModelRefreshResult,
	reflectTypeOf[OAuthCompletion]():               sampleOAuthCompletion,
	reflectTypeOf[OAuthStartResult]():              sampleOAuthStartResult,

	reflectTypeOf[time.Time](): sampleTime,
}

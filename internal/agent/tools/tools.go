package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/fsext"
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
	// DepthContextKey is the key for how many delegation levels below a
	// person's own turn this turn is running: 0 for a session someone
	// drives directly, 1 inside a delegation that session started, and so
	// on. Reacting to a delegation's result does not deepen it — that is
	// the same session at the same level — so a session may delegate as
	// many times in a row as the work needs. The delegation tools read
	// this to refuse nesting past the limit; see
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

// GetDepthFromContext retrieves how many delegation levels below a
// person's own turn this turn runs. Absent (0) means a session someone
// drives directly.
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

// errSennitIO marks a failure as Sennit's own file I/O going wrong —
// opening or stat-ing a log file, writing or seeking a scratch file it
// created for itself — rather than something the caller's arguments
// caused. It is not something the model can react to (there is no
// argument to correct), so a caller wraps an error with it via a second
// %w verb and checks errors.Is against it to route the failure to a Go
// error instead of a text response. See the error-vs-response rule next
// to "Tools are self-documenting" in AGENTS.md.
var errSennitIO = errors.New("sennit I/O error")

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
func requirePermission(ctx context.Context, perms permission.Requester, req permission.CreatePermissionRequest) (resp fantasy.ToolResponse, denied bool, err error) {
	if perms == nil {
		return fantasy.ToolResponse{}, false, nil
	}
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
// an absolute path (e.g. an unreadable cwd); the returned absPath is the
// plain (symlink-unaware) absolute form, so callers still open exactly the
// path they asked for. resolvedPath is the symlink-followed form the
// outside test itself is computed from — the actual destination, which a
// permission request must show and key on rather than absPath: a directory
// symlink lexically inside workingDir makes absPath and resolvedPath
// diverge, and absPath alone would let the dialog claim a location the
// request is not really about. resolvedPath equals absPath whenever no
// symlink is involved, the common case.
//
// The outside test itself resolves symlinks first: without that, a
// directory symlink lexically inside workingDir (e.g. "up" -> "../..",
// created with the bash tool, whose relative target is invisible to the
// static confinement check that gates bash) would pass the old
// filepath.Abs-only prefix test while actually landing outside workingDir
// once the OS follows the link. Resolution failing for any reason other
// than "the path doesn't exist yet" (permission denied, a symlink loop, ...)
// is treated as outside rather than surfaced as err, so a confinement check
// fails closed instead of either erroring out or, worse, treating an
// unresolvable path as safe; resolvedPath falls back to absPath in that
// case, since no better destination is known.
func resolveWithinWorkdir(workingDir, path string) (absPath, resolvedPath string, outside bool, err error) {
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", "", false, fmt.Errorf("resolving working directory: %w", err)
	}
	absPath, err = filepath.Abs(path)
	if err != nil {
		return "", "", false, fmt.Errorf("resolving path: %w", err)
	}

	resolvedWorkingDir, err := resolveExistingAncestorSymlinks(absWorkingDir)
	if err != nil {
		return absPath, absPath, true, nil
	}
	resolvedPath, err = resolveExistingAncestorSymlinks(absPath)
	if err != nil {
		return absPath, absPath, true, nil
	}

	// fsext.HasPrefix treats a sibling like "..foo" as inside workingDir,
	// unlike a bare strings.HasPrefix(relPath, "..") check, which would
	// mistake it for an escape via "..".
	outside = !fsext.HasPrefix(resolvedPath, resolvedWorkingDir)
	return absPath, resolvedPath, outside, nil
}

// requireOutsideWorkdirPermission is the "path resolved outside the working
// directory" gate glob, grep, ripgrep, ls, and read/multi_read each ran as
// an identical seven-line block (only the tool name, the permission Action,
// the outsideWorkdirNotice verb, the missingSessionID wording, and the
// *PermissionsParams type differed): look up the session ID this call is
// running under — there is nothing model-recoverable about a missing one
// (it is wired in by the caller, not a tool argument), so that case returns
// a Go error via missingSessionID rather than a tool response — then build
// the notice outsideWorkdirNotice shows the user and ask permission.Requester
// for it.
//
// The price of leaving this duplicated rather than shared was not
// hypothetical: commit 99efb4886 had to repeat the same seven-line diff
// across all five files, and missed a sixth call site (download.go) doing
// the same thing, which is why that omission could stand as its own bug.
//
// The (resp, denied, err) return mirrors requirePermission's: denied is
// only meaningful when err is nil. A caller with its own response wrapper
// (read_core.go's readCoreResult) builds it from resp rather than this
// helper reaching into that type — and read_core.go additionally needs a
// session ID even when the path is not outside the working directory (it
// records file-tracking history against it), so it keeps its own
// unconditional check ahead of the "outside" branch and calls this helper
// only for the permission request itself, once a session ID is already
// known to exist.
//
// web_fetch.go and web_search.go run a similar-looking block, but on a
// completely different trigger (permissions != nil, not "resolved outside
// the working directory") with no path to resolve at all (Path is always
// workingDir) — folding them in here would force a needless path argument
// through every caller, so they keep their own copy.
func requireOutsideWorkdirPermission(
	ctx context.Context,
	perms permission.Requester,
	call fantasy.ToolCall,
	toolName, action, verb, missingSessionIDAction string,
	requestedAbs, resolvedAbs string,
	params any,
) (resp fantasy.ToolResponse, denied bool, err error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, false, missingSessionID(missingSessionIDAction)
	}
	path, description := outsideWorkdirNotice(verb, requestedAbs, resolvedAbs)
	return requirePermission(ctx, perms, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        path,
		ToolCallID:  call.ID,
		ToolName:    toolName,
		Action:      action,
		Description: description,
		Params:      params,
	})
}

// outsideWorkdirNotice builds the Path and Description a permission request
// raises for a path resolveWithinWorkdir reported outside — always keyed
// and displayed on resolvedAbs, the real destination, so a persistent grant
// cannot outlive a symlink being repointed and the dialog cannot show a
// location the request is not actually about. requestedAbs is folded into
// the description only when it differs from resolvedAbs — a symlink hop
// happened — since that is the one case where the plain path a user typed
// or the model asked for would otherwise be missing from the prompt
// entirely. When the two are equal, the description is byte-identical to
// the unresolved form it replaces.
func outsideWorkdirNotice(verb, requestedAbs, resolvedAbs string) (path, description string) {
	if resolvedAbs == requestedAbs {
		return resolvedAbs, fmt.Sprintf("%s: %s", verb, resolvedAbs)
	}
	return resolvedAbs, fmt.Sprintf("%s: %s (requested as %s)", verb, resolvedAbs, requestedAbs)
}

// resolveExistingAncestorSymlinks resolves symlinks in the longest existing
// ancestor of path and re-appends whatever remainder does not exist yet.
// filepath.EvalSymlinks fails outright on a path that doesn't exist, and the
// common case here is a write's target — a file that is about to be
// created inside a directory that does — so resolving the whole path
// straight through would reject that ordinary case.
//
// EvalSymlinks also fails with ENOENT for a dangling symlink (one whose
// target is absent), which looks identical to "this component doesn't
// exist yet" unless the two are told apart: os.Lstat succeeds on a
// dangling link (it stats the link itself, not its target) and only fails
// with ErrNotExist when the name is genuinely absent. So on ENOENT, Lstat
// the component first — present-but-unresolvable (a dangling link, or a
// non-directory partway through the path) is reported as an error rather
// than folded into the remainder, which the caller turns into "outside".
// Without that check, "ln -s ../../evil up" (relative, so invisible to the
// bash confinement check) then "write up/foo.txt" would resolve "up" as an
// as-yet-nonexistent ordinary path component, report the write as inside,
// and let the write's own MkdirAll follow the link out.
func resolveExistingAncestorSymlinks(path string) (string, error) {
	remainder := ""
	dir := path
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if _, lerr := os.Lstat(dir); lerr == nil {
			// The component is there — it just can't be resolved (a
			// dangling symlink, most likely). Fail closed rather than
			// treat it as "doesn't exist yet".
			return "", err
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			return "", lerr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked up to the filesystem root and even that could not be
			// resolved; nothing left to try.
			return "", err
		}
		remainder = filepath.Join(filepath.Base(dir), remainder)
		dir = parent
	}
}

// ensureParentDir creates the parent directory of filePath, as needed before
// writing a new file.
func ensureParentDir(filePath string) error {
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("failed to create parent directories: %w", err)
	}
	return nil
}

// recordFileHistory updates the file history for a write that has already
// landed on disk. Writing is the caller's job and deliberately so: every
// mutation goes through applyFileMutation, which writes with
// fsext.AtomicWriteFileIfUnchanged so a file edited between the diff the
// user approved and the commit is refused rather than overwritten. A
// combined write-and-record helper used to sit here and is gone, because
// the only way to use it was to give that check up.
func recordFileHistory(ctx context.Context, files FileHistory, sessionID, filePath, oldContent, newContent string) error {
	if files == nil || sessionID == "" {
		return nil
	}
	content, found, err := files.LatestContent(ctx, sessionID, filePath)
	switch {
	case err != nil:
		// A real lookup failure, not "no such row". Treating every error
		// as "nothing recorded" wrote a fresh baseline over a history
		// that may well exist, which is how an undo loses the version it
		// was meant to restore. The write itself already landed, so this
		// reports rather than pretending it did not happen.
		return fmt.Errorf("read file history: %w", err)
	case !found:
		// Nothing recorded for this path in this session yet: the content
		// that was on disk before this write becomes the baseline. The
		// two branches are exclusive — writing both would store
		// oldContent twice in a row.
		if err := files.CreateVersion(ctx, sessionID, filePath, oldContent); err != nil {
			return fmt.Errorf("error creating file history: %w", err)
		}
	case content != oldContent:
		// User manually changed the content; store an intermediate version.
		if err := files.CreateVersion(ctx, sessionID, filePath, oldContent); err != nil {
			return fmt.Errorf("create intermediate file history: %w", err)
		}
	}
	if err := files.CreateVersion(ctx, sessionID, filePath, newContent); err != nil {
		return fmt.Errorf("create committed file history: %w", err)
	}

	return nil
}

// recordWholeFileRead marks the file as fully seen by this session. Only
// for writes where the caller supplied the entire content (Write, and
// creating a file through Edit/Multi-Edit): it saw everything it just
// wrote.
func recordWholeFileRead(ctx context.Context, filetracker FileTracking, sessionID, filePath string) {
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
func recordEditedSpan(ctx context.Context, filetracker FileTracking, sessionID, filePath, oldContent, newContent string) {
	start, end, ok := changedLineSpan(oldContent, newContent)
	if !ok {
		return
	}
	delta := strings.Count(newContent, "\n") - strings.Count(oldContent, "\n")
	filetracker.RecordEdit(ctx, sessionID, filePath, start, end, max(start, end+delta))
}

// NewHTTPClient builds an http.Client tuned for outbound fetch/download
// tools: a shared transport with modest idle-connection pooling, and the
// given overall request timeout (fetch and download use different
// timeouts; web_fetch reuses fetch's). CheckRedirect is set so a redirect
// cannot silently carry the request off to an address the user never
// approved — see checkFetchRedirect. Exported so internal/agent's own
// agentic-fetch fallback client (delegation_finalizer.go's fetchClient) can
// share it instead of hand-rolling an identical transport that misses this
// guard.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 10
	transport.IdleConnTimeout = 90 * time.Second

	return &http.Client{
		Timeout:       timeout,
		Transport:     transport,
		CheckRedirect: checkFetchRedirect,
	}
}

// errBlockedRedirect explains why a fetch/download tool refused to follow a
// redirect. The URL a fetch/download call starts with is one the user has
// seen and approved through permission.Requester; a redirect target is not
// — the server can send the request wherever it likes after that approval
// is granted. Fetching a user's own loopback service ("fetch my local dev
// server on localhost:3000") is a legitimate request, so the initial
// address is never checked; only a hop that leaves the approved host and
// lands on a loopback, private, or link-local address is refused.
var errBlockedRedirect = errors.New("refusing to follow a redirect to a private, loopback, or link-local address")

// checkFetchRedirect is installed as the shared fetch/download client's
// CheckRedirect. Go's http.Client never calls CheckRedirect for the first
// request, only for each hop after a redirect (via[0] is always that first,
// user-approved request), so this only ever restricts hops the user did not
// see. A redirect that stays on the originally approved host is let
// through unchanged — a local dev server redirecting to another path or
// port on the same host is exactly what an approved "fetch localhost:3000"
// expects. A redirect that leaves that host and resolves to a blocked
// address is refused.
func checkFetchRedirect(req *http.Request, via []*http.Request) error {
	// Matches net/http's own default cap (checkRedirect in client.go);
	// overriding CheckRedirect drops that default, so it must be
	// reinstated here.
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	origHost := via[0].URL.Hostname()
	newHost := req.URL.Hostname()
	if strings.EqualFold(newHost, origHost) {
		return nil
	}
	return checkRedirectHostAllowed(req.Context(), newHost)
}

// checkRedirectHostAllowed resolves host and refuses the redirect if any
// resolved address is loopback, private, link-local, or unspecified. A
// lookup failure is not blocked here — it is left to surface at dial time
// as the same "failed to fetch/download" error the model already sees for
// any unreachable URL.
func checkRedirectHostAllowed(ctx context.Context, host string) error {
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedRedirectAddr(ip) {
			return fmt.Errorf("%w: %s", errBlockedRedirect, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil
	}
	for _, addr := range addrs {
		if isBlockedRedirectAddr(addr.IP) {
			return fmt.Errorf("%w: %s", errBlockedRedirect, host)
		}
	}
	return nil
}

// isBlockedRedirectAddr reports whether ip is one a redirect must not be
// allowed to carry an approved request to.
func isBlockedRedirectAddr(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

type toolAvailabilityOption func(*toolAvailability)

type ToolAvailabilityOption = toolAvailabilityOption

type toolAvailability struct {
	ghAvailable bool
}

func withGHAvailability(available bool) toolAvailabilityOption {
	return func(availability *toolAvailability) {
		availability.ghAvailable = available
	}
}

func ResolveSystemToolAvailability() toolAvailabilityOption {
	return withGHAvailability(commandAvailable(exec.LookPath, "gh"))
}

func applyToolAvailability(options []toolAvailabilityOption) toolAvailability {
	var availability toolAvailability
	for _, option := range options {
		if option != nil {
			option(&availability)
		}
	}
	return availability
}

func commandAvailable(lookup func(string) (string, error), name string) bool {
	_, err := lookup(name)
	return err == nil
}

// toolDescriptionData is the common data structure for tool description templates.
type toolDescriptionData struct {
	GhAvailable bool
}

// renderToolDescription renders a tool description template with the given data.
func renderToolDescription(tmpl *template.Template) string {
	return renderToolDescriptionWithAvailability(tmpl, toolAvailability{})
}

func renderToolDescriptionWithAvailability(tmpl *template.Template, availability toolAvailability) string {
	data := toolDescriptionData{GhAvailable: availability.ghAvailable}
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
func confinementRefusal(permissions permission.Requester, filePath string) (message string, ok bool) {
	if permissions == nil {
		return "", false
	}
	dir := permissions.ConfinedDir()
	if dir == "" {
		return "", false
	}
	abs, _, outside, err := resolveWithinWorkdir(dir, filePath)
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

// derefResponse unwraps an optional tool response, for the call sites
// that forward a helper's "return this as-is" result. A nil response is
// the zero one, which is what a tool returns alongside a non-nil error.
func derefResponse(resp *fantasy.ToolResponse) fantasy.ToolResponse {
	if resp == nil {
		return fantasy.ToolResponse{}
	}
	return *resp
}

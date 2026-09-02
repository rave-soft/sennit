package tools

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/rave-soft/sennit/internal/brand"
)

const SennitLogsToolName = brand.ToolLogs

//go:embed sennit_logs.md.tpl
var sennitLogsDescriptionTmpl []byte

var sennitLogsDescriptionTpl = template.Must(
	template.New("sennitLogsDescription").
		Parse(string(sennitLogsDescriptionTmpl)),
)

type sennitLogsDescriptionData struct {
	DefaultLines int
	MaxLines     int
}

func sennitLogsDescription() string {
	return renderTemplate(sennitLogsDescriptionTpl, sennitLogsDescriptionData{
		DefaultLines: defaultLogLines,
		MaxLines:     maxLogLines,
	})
}

// Max line size to prevent memory issues with very long log lines (1 MB).
const maxLogLineSize = 1024 * 1024

// Default and max line limits. maxLogLines is the *safety* cap a single call
// may return: the cursor paginates within it, so a large diagnostic is
// retrieved with a few capped calls rather than one unbounded call, and the
// cap never inflates the call count for the ordinary last-100 case (limit 100
// < cap 500). The effective per-call default is defaultLogLines (100), the
// value the description promises and resolveLimit applies when the caller gives
// neither limit nor lines.
const (
	defaultLogLines = 100
	maxLogLines     = 500
)

// maxChainLines bounds a single correlated-chain call. A chain is meant to be
// a *minimal* useful trace (the diagnostic anchor lines for one run/session),
// not an unbounded dump, so it has its own tighter cap than maxLogLines.
const maxChainLines = 100

// maxScanBytes bounds how many bytes scanBackward will read from the scan
// start before it must stop counting. It is a package-level var (not a const)
// so tests can shrink it to a few hundred bytes and exercise the "scan budget
// exhausted" path without materializing a multi-GB log; production leaves it at
// the default. The budget counts EVERY byte read from the scan start (page and
// behind alike), so a pathological multi-GB log cannot run the tool forever.
// When the budget is exhausted, match_count is a lower bound and the scan is
// reported as truncated (scanned_truncated), never silently treated as exact.
var maxScanBytes = int64(64 * 1024 * 1024)

// Reserved fields that should not appear as extra key=value pairs.
// Case-insensitive matching is used.
var reservedFields = map[string]bool{
	"time":   true,
	"level":  true,
	"source": true,
	"msg":    true,
}

// Sensitive field keys that should be redacted (matched case-insensitively).
var sensitiveKeys = []string{
	"authorization",
	"api-key",
	"api_key",
	"apikey",
	"token",
	"secret",
	"password",
	"credential",
}

// chainAnchorMsgs are the *diagnostic* log messages a correlated chain is made
// of, and the honest scope of a chain. Every one of these is a real production
// log line that carries session_id and run_id (T3 provider correlation and T4
// repair correlation; the carried-history trim line also carries them since T5),
// so a chain can filter them by one session_id/run_id:
//
//   - Provider request started / Provider request finished: one pair per
//     provider attempt (T3), the attempt and outcome.
//   - Provider request failed, retrying: the retry warning between attempts
//     (T3), tying the failed attempt to the one that re-runs.
//   - Trimmed the carried sub-agent session to the budget: the carried-history
//     trim for a delegation (T1, correlated by session/run since T5).
//   - Dropping orphaned tool result with no matching tool call / Injecting
//     synthetic tool result for orphaned tool call: the orphan-exchange repairs
//     (T4).
//   - Tool lifecycle: the actual tool-call and tool-result callbacks, correlated
//     without recording tool arguments or result content.
//
// A chain is a minimal, cheap trace of a run's provider calls, Tool lifecycle,
// and history handling; a reader that needs every routine INFO line drops the
// chain and uses the filters.
var chainAnchorMsgs = map[string]bool{
	"Provider request started":                                 true,
	"Provider request finished":                                true,
	"Provider request failed, retrying":                        true,
	"Trimmed the carried sub-agent session to the budget":      true,
	"Dropping orphaned tool result with no matching tool call": true,
	"Injecting synthetic tool result for orphaned tool call":   true,
	"Tool lifecycle": true,
}

type SennitLogsParams struct {
	Lines     int    `json:"lines,omitempty" description:"Number of recent log entries to return (0 uses the default). Alias of limit kept for compatibility."`
	Limit     int    `json:"limit,omitempty" description:"Max entries to return this call (0 uses the default). Ignored when chain is set."`
	Level     string `json:"level,omitempty" description:"Only entries at this level. Must be one of DEBUG, INFO, WARN, or ERROR (case-insensitive)." enum:"DEBUG,INFO,WARN,ERROR"`
	Component string `json:"component,omitempty" description:"Only entries whose component field equals this value (sparse; few lines carry it)."`
	Contains  string `json:"contains,omitempty" description:"Case-insensitive substring that must appear in the entry's message or any (redaction-safe) field value."`
	SessionID string `json:"session_id,omitempty" description:"Only entries whose session_id field equals this value."`
	RunID     string `json:"run_id,omitempty" description:"Only entries whose run_id field equals this value."`
	Since     string `json:"since,omitempty" description:"Only entries at or after this time. An RFC3339 timestamp (e.g. 2024-01-15T10:30:00Z) or a duration relative to now (e.g. 5m, 1h); anything else is an error."`
	Cursor    string `json:"cursor,omitempty" description:"Continue from a previous response's next_cursor. Empty starts at the end (most recent first). A stale cursor (rotated/replaced file) returns an empty page; start over without it."`
	Chain     bool   `json:"chain,omitempty" description:"Return the correlated diagnostic chain (provider request/retry, carried-history trim, orphan repair) for the given session_id/run_id, most recent first. Implies a limit."`
}

// SennitLogsResponseMetadata is the structured metadata returned alongside the
// (backward-compatible) text output.
//
// matchCount is the number of entries counted as matching for the current
// filter. matchCountExact says that count is the *exact* total for the whole
// file: it is true when the backward scan reached the start of the file (so
// nothing older was left unexamined). When the scan stopped early - the byte
// budget ran out, or a cursor's continuation ran off the end - matchCountExact
// is false and matchCount is a *lower bound* (at least this many match), with
// scannedTruncated stating that the scan itself was cut short. This keeps the
// "how many match" answer honest: a reader can tell "exactly N" from "at least
// N".
//
// shownCount is how many were rendered in this call. truncated says a matching
// entry older than this page was actually found (so older matches exist); it is
// set only by observing a real older match, never merely by the scan stopping.
// nextCursor is the token for the next call (empty when there is nothing older
// to fetch).
type SennitLogsResponseMetadata struct {
	MatchCount       int    `json:"match_count"`
	MatchCountExact  bool   `json:"match_count_exact"`
	ScannedTruncated bool   `json:"scanned_truncated,omitempty"`
	ShownCount       int    `json:"shown_count"`
	Truncated        bool   `json:"truncated"`
	NextCursor       string `json:"next_cursor,omitempty"`
	ChainMode        bool   `json:"chain_mode,omitempty"`
	ChainSession     string `json:"chain_session,omitempty"`
	ChainRun         string `json:"chain_run,omitempty"`
}

func NewSennitLogsTool(logFile string) fantasy.AgentTool {
	tool := fantasy.NewParallelAgentTool(
		SennitLogsToolName,
		sennitLogsDescription(),
		func(ctx context.Context, params SennitLogsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			// Cancellation is not model-recoverable: it means the run
			// itself is over, not that this call failed and can be
			// retried. Checked before any of runSennitLogs's own
			// failures, so a canceled call never turns into a normal
			// tool result.
			if err := ctx.Err(); err != nil {
				return fantasy.ToolResponse{}, err
			}
			output, metadata, err := runSennitLogs(logFile, params)
			if err != nil {
				// Opening/stat-ing the log file is Sennit's own I/O, not
				// something the model's arguments caused — an
				// infrastructure failure, unlike an invalid cursor/level/
				// since/limit, which stays a text response below.
				if errors.Is(err, errSennitIO) {
					return fantasy.ToolResponse{}, fmt.Errorf("sennit logs: %w", err)
				}
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(output), metadata), nil
		},
	)
	info := tool.Info()
	for _, name := range []string{"lines", "limit"} {
		if parameter, ok := info.Parameters[name].(map[string]any); ok {
			parameter["minimum"] = 0
			parameter["maximum"] = maxLogLines
		}
	}
	tool = withToolParameterSchema(toolInfoOverride{AgentTool: tool, info: info}, map[string]toolParameterSchema{"level": {enum: []string{"DEBUG", "INFO", "WARN", "ERROR"}}})
	return withToolRootSchema(tool, map[string]any{
		"if": map[string]any{"required": []string{"chain"}, "properties": map[string]any{"chain": map[string]any{"const": true}}},
		"then": map[string]any{"anyOf": []any{
			map[string]any{"required": []string{"session_id"}, "properties": map[string]any{"session_id": map[string]any{"type": "string", "pattern": `.*\S.*`}}},
			map[string]any{"required": []string{"run_id"}, "properties": map[string]any{"run_id": map[string]any{"type": "string", "pattern": `.*\S.*`}}},
		}},
	})
}

// runSennitLogs reads, filters and paginates the log file, returning the
// backward-compatible text output and its structured metadata. It returns an
// error only for I/O failures; a missing/empty file is reported in the text.
func runSennitLogs(logFile string, params SennitLogsParams) (string, SennitLogsResponseMetadata, error) {
	// Validate every caller-controlled parameter before touching the filesystem.
	// In particular, missing and empty logs must not hide a bad filter/cursor.
	if err := validateLogParams(params); err != nil {
		return "", SennitLogsResponseMetadata{}, err
	}

	info, err := os.Stat(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "No log file found", SennitLogsResponseMetadata{}, nil
		}
		return "", SennitLogsResponseMetadata{}, fmt.Errorf("error accessing log file: %w (%w)", err, errSennitIO)
	}
	if info.Size() == 0 {
		return "Log file is empty", SennitLogsResponseMetadata{}, nil
	}

	f, err := os.Open(logFile)
	if err != nil {
		return "", SennitLogsResponseMetadata{}, fmt.Errorf("error opening log file: %w (%w)", err, errSennitIO)
	}
	defer f.Close()

	size, err := f.Stat()
	if err != nil {
		return "", SennitLogsResponseMetadata{}, fmt.Errorf("error statting log file: %w (%w)", err, errSennitIO)
	}

	filt, _ := newLogFilter(params) // validated before the filesystem access

	// Resolve the scan start: the cursor's byte offset if present, else the
	// end of the file. A cursor is bound to a file *generation* (block 1): it
	// records the (dev, inode) when the platform exposes them, else a
	// size/mtime fallback identity. On continuation we verify the current file
	// is the same generation; a mismatch (rotation to a new inode, a path reused
	// for a different log) or a cursor offset that now exceeds the file size
	// means the cursor is stale - we return an EMPTY page with that metadata
	// (truncated=false, no next_cursor) and do NOT emit new generation records,
	// so a stale token can never be mistaken for fresh log content.
	start := size.Size()
	boundary := int64(-1)
	var returned int64 // matches already returned by earlier pages (from cursor)
	if params.Cursor != "" {
		curOffset, curReturned, curID, derr := decodeCursor(params.Cursor)
		if derr != nil {
			return "", SennitLogsResponseMetadata{}, fmt.Errorf("invalid cursor: %w", derr)
		}
		// A stale cursor is not an error (rotation/reuse is normal): it is an
		// empty continuation. The offset is clamped into [0, size] by
		// decodeCursor; if it landed at the end (offset == size) there is
		// nothing to read and the identity is, by construction, no longer the
		// file the offset was minted for.
		stale := curOffset >= size.Size()
		if !stale {
			same, ierr := curID.matches(f, size, curOffset)
			if ierr != nil {
				return "", SennitLogsResponseMetadata{}, fmt.Errorf("verifying cursor identity: %w (%w)", ierr, errSennitIO)
			}
			stale = !same
		}
		if stale {
			// Empty stale page: no records, no next cursor, not truncated. The
			// metadata still says the cursor was a continuation (shown_count 0)
			// so a caller can tell "ran off the end / file rotated" from a fresh
			// no-cursor call.
			meta := SennitLogsResponseMetadata{MatchCount: 0, MatchCountExact: true, ShownCount: 0, Truncated: false}
			return "No more log entries to continue from this cursor (the log rotated or was replaced); start a fresh call without a cursor", meta, nil
		}
		start = curOffset
		returned = curReturned
		boundary = start
	}

	limit := resolveLimit(params)

	// Scan backwards from start, collecting matching entries and counting the
	// matches behind the page (for matchCount). With a cursor, the scan stops
	// once the page is full and it has crossed the boundary; without one, it
	// returns the most recent `limit` matches.
	result := scanBackward(f, start, boundary, filt, limit)

	// Chain mode: drop non-anchor messages and re-clamp to its own cap. The
	// anchor filter is applied after the backward scan so the cursor still
	// paginates over raw entries (stable), but only anchors are returned.
	shown := result.entries
	if params.Chain {
		shown = filterChainAnchors(shown)
	}

	// matchCount is the matches counted for the current (file, filter): the
	// matches earlier pages already returned (returned, carried in the cursor)
	// plus this page (shown) plus the matches still behind it (matchedBehind).
	// It is stable across a pagination walk, which is what "how many match"
	// should mean.
	matchCount := int(returned + int64(len(shown)) + int64(result.matchedBehind))
	// match_count_exact is true only when the scan examined the whole file
	// (reached the start without stopping on the byte budget); otherwise the
	// count is a lower bound and scanned_truncated states the scan was cut
	// short (block 3).
	matchCountExact := result.reachedStart
	scannedTruncated := !result.reachedStart
	truncated := result.truncated

	// nextCursor: a cursor at the byte offset of the page's OLDEST entry,
	// carrying the running match count (returned + this page) and the file
	// generation identity so the continuation can verify it is the same file.
	// Only present when truncated (an older MATCHING entry was actually found)
	// and the page is non-empty. `shown` is chronological (oldest first), so
	// the oldest is index 0.
	var nextCursor string
	if truncated && len(shown) > 0 {
		cursorID, ierr := newFileIdentity(f, size, shown[0].offset)
		if ierr != nil {
			return "", SennitLogsResponseMetadata{}, fmt.Errorf("creating cursor identity: %w (%w)", ierr, errSennitIO)
		}
		nextCursor = encodeCursor(shown[0].offset, returned+int64(len(shown)), cursorID)
	}

	meta := SennitLogsResponseMetadata{
		MatchCount:       matchCount,
		MatchCountExact:  matchCountExact,
		ScannedTruncated: scannedTruncated,
		ShownCount:       len(shown),
		Truncated:        truncated,
		NextCursor:       nextCursor,
		ChainMode:        params.Chain,
	}
	if params.Chain {
		meta.ChainSession = params.SessionID
		meta.ChainRun = params.RunID
	}

	if len(shown) == 0 {
		// Distinguish "no matches at all" from "matches exist but all beyond
		// this cursor" so a continuation call that runs off the end is not
		// mistaken for an empty file.
		if matchCount > 0 {
			return "No more log entries match the current filter beyond this point", meta, nil
		}
		return "No log entries match the current filter", meta, nil
	}

	return formatLogEntries(shown, meta) + "\n", meta, nil
}

// resolveLimit picks the effective per-call limit. In chain mode the chain cap
// applies (and is independent of lines/limit). Otherwise limit (default 100)
// wins, with lines as a backward-compatible alias, and maxLogLines as the hard
// safety cap. The default (when neither limit nor lines is supplied) is
// defaultLogLines = 100, consistent with the description.
func resolveLimit(params SennitLogsParams) int {
	if params.Chain {
		return maxChainLines
	}
	limit := params.Limit
	if limit <= 0 {
		limit = params.Lines
	}
	if limit <= 0 {
		limit = defaultLogLines
	}
	return limit
}

// validateLogParams checks every caller-controlled parameter before the caller
// stats or opens the log. This makes invalid requests deterministic even when
// the log path is missing or empty.
func validateLogParams(params SennitLogsParams) error {
	limit := resolveLimit(params)
	if err := validateLimit(params, limit); err != nil {
		return err
	}
	if _, err := newLogFilter(params); err != nil {
		return err
	}
	if params.Chain && strings.TrimSpace(params.SessionID) == "" && strings.TrimSpace(params.RunID) == "" {
		return fmt.Errorf("invalid chain: session_id or run_id is required")
	}
	if params.Cursor != "" {
		if _, _, _, err := decodeCursor(params.Cursor); err != nil {
			return fmt.Errorf("invalid cursor: %w", err)
		}
	}
	return nil
}

// validateLimit enforces the limit's range as a *text error* (block 6) rather
// than a silent clamp. An explicitly-supplied limit above maxLogLines is
// rejected so the model learns the cap instead of being silently reduced; a
// limit of 0 (neither limit nor lines set) is the default path, not an error.
// The supplied aliases are validated even in chain mode, where the effective
// cap is fixed, so invalid input is never silently ignored.
func validateLimit(params SennitLogsParams, limit int) error {
	for name, value := range map[string]int{"lines": params.Lines, "limit": params.Limit} {
		if value < 0 {
			return fmt.Errorf("invalid %s %d: must be at least 0", name, value)
		}
		if value > maxLogLines {
			return fmt.Errorf("invalid %s %d: max is %d", name, value, maxLogLines)
		}
	}
	if limit > maxLogLines { // defensive if resolution changes.
		return fmt.Errorf("invalid limit %d: max is %d", limit, maxLogLines)
	}
	return nil
}

// logFilter holds the parsed, cheaply-testable predicate state for one call.
// sinceTime, when non-nil, is the lower time bound; entries older than it (or
// without a parseable time) are excluded.
type logFilter struct {
	level        string // normalized level, "" = any
	component    string // "" = any
	contains     string // lower-cased substring, "" = any
	sessionID    string // "" = any
	runID        string // "" = any
	sinceTime    *time.Time
	chain        bool
	chainAnchors map[string]bool
	// accept is an optional tool-specific predicate applied after standard filters.
	accept func(map[string]any) bool
	// observe is called for every matching record, including records behind a full page.
	observe func(logRecord)
}

// newLogFilter builds the filter predicate for one call, validating the
// level and since parameters. An invalid (non-empty, unrecognized) level or a
// since that is neither an RFC3339 timestamp nor a Go duration is a caller
// mistake, so it is returned as an error - the tool reports it as a text error
// (block 6) instead of silently filtering with the wrong predicate.
func newLogFilter(params SennitLogsParams) (*logFilter, error) {
	level, err := normalizeLevelFilter(params.Level)
	if err != nil {
		return nil, err
	}
	f := &logFilter{
		level:     level,
		component: params.Component,
		sessionID: params.SessionID,
		runID:     params.RunID,
		chain:     params.Chain,
	}
	if params.Contains != "" {
		f.contains = strings.ToLower(params.Contains)
	}
	if params.Chain {
		f.chainAnchors = chainAnchorMsgs
	}
	if params.Since != "" {
		t, ok := parseSince(params.Since)
		if !ok {
			return nil, fmt.Errorf("invalid since %q: expected an RFC3339 timestamp (e.g. 2024-01-15T10:30:00Z) or a duration (e.g. 5m, 1h)", params.Since)
		}
		f.sinceTime = &t
	}
	return f, nil
}

// normalizeLevelFilter maps a user level to the tool's canonical token. An
// empty value means "no level filter" and returns "". A non-empty value that
// is not one of DEBUG/INFO/WARN/ERROR is a caller mistake and is returned as
// an error rather than silently treated as "any level". The accepted values
// deliberately match the advertised enum exactly.
func normalizeLevelFilter(level string) (string, error) {
	switch strings.TrimSpace(level) {
	case "":
		return "", nil
	case "DEBUG":
		return "DEBUG", nil
	case "INFO":
		return "INFO", nil
	case "WARN":
		return "WARN", nil
	case "ERROR":
		return "ERROR", nil
	default:
		return "", fmt.Errorf("invalid level %q: must be one of DEBUG, INFO, WARN, or ERROR", level)
	}
}

// parseSince parses a since value as an RFC3339 absolute time or a Go
// duration relative to now (e.g. "5m", "1h30m"). It returns ok=false when the
// value is neither (in which case the filter ignores since rather than erroring,
// so a malformed since degrades to "no time bound" instead of failing the call).
func parseSince(s string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), true
	}
	return time.Time{}, false
}

// logRecord is one parsed log entry plus the file-position facts the cursor
// needs. offset is the byte index of the line's start; size is the line's byte
// length (including its trailing newline when present). The fields are kept
// separately (not a map) so the filter and the cursor never re-parse the JSON.
type logRecord struct {
	offset  int64
	size    int64
	entry   map[string]any
	t       time.Time
	hasTime bool
	level   string
}

// backwardScanResult is the output of scanBackward. entries is the rendered
// page in chronological order (oldest first), ready to format. matchedBehind is
// the number of matching entries strictly older than the page (behind the
// start boundary) that were counted but not rendered.
//
// truncated reports whether a matching entry *older than the page* was actually
// found (block 2: "more matches exist" is proven by seeing one, not by the
// scan stopping). reachedStart reports whether the scan walked all the way to
// the start of the file: when false, the scan stopped early (byte budget
// exhausted, or it ran off the end on a cursor continuation) and matchedBehind
// is only a lower bound, so match_count is not exact.
type backwardScanResult struct {
	entries       []logRecord
	matchedBehind int
	truncated     bool
	reachedStart  bool
}

// scanBackward scans the file backwards from start (a byte offset, normally the
// file end or a cursor) collecting logRecords. boundary is the byte offset of
// the cursor when one was supplied, or -1 when there is none:
//
//   - boundary >= 0 (a cursor was given): the scan starts at the cursor's
//     offset and skips the single line the cursor points at (the previous
//     page's oldest entry, already returned); the rest are the next page.
//     Appends after the cursor are beyond `start` and are never scanned, which
//     is what makes the cursor stable under appends.
//
//   - boundary < 0 (no cursor): the scan starts at the end of the file and
//     returns the most recent `limit` matches, setting truncated when the file
//     held more.
//
// It reads in 8KB chunks and reuses the pre-T5 remainder-carrying logic for
// lines that straddle a chunk boundary (pinned by
// TestReadLastLinesKeepsEntriesAcrossChunkBoundaries). It is memory-bounded to
// one chunk at a time. The page itself is O(page + the gap of non-matching
// lines immediately behind it); counting the total match_count additionally
// scans older bytes, bounded by maxScanBytes so a pathological multi-GB file
// cannot run the tool forever. When the byte budget is exhausted, match_count
// is a lower bound (the scan stopped before EOF).
func scanBackward(f *os.File, start, boundary int64, filt *logFilter, limit int) backwardScanResult {
	const chunkSize = 8192 // 8KB chunks
	pos := start
	var remainder []byte
	var page []logRecord // collected most-recent-first, reversed at the end
	matchedBehind := 0
	truncated := false // true only when an older MATCHING entry was actually found
	done := false
	reachedStart := false
	incomplete := false // an oversized line was discarded without being fully read
	firstChunk := true  // true only for the very first chunk read by this call
	// bytesScanned counts every byte examined from the scan start (page and
	// behind alike). It is the budget: once it reaches maxScanBytes the scan
	// stops, so a pathological file cannot run the tool forever and match_count
	// becomes a lower bound (reported via reachedStart=false).
	budget := maxScanBytes
	bytesScanned := int64(0)

	// stopNow reports whether the byte budget is exhausted; the caller then
	// breaks out of the scan without having reached the start of the file.
	stopNow := func() bool { return bytesScanned >= budget }

	for pos > 0 && !done {
		chunkStart := max(pos-chunkSize, 0)
		chunkLen := int(pos - chunkStart)
		if chunkLen == 0 {
			break
		}
		// Budget check BEFORE reading this chunk: if the budget is already
		// spent, stop (we did not reach the start).
		if stopNow() {
			break
		}
		if _, err := f.Seek(chunkStart, 0); err != nil {
			return backwardScanResult{}
		}
		chunk := make([]byte, chunkLen)
		if _, err := io.ReadFull(f, chunk); err != nil {
			return backwardScanResult{}
		}
		pos = chunkStart
		bytesScanned += int64(chunkLen)

		// remainder is the head of a line whose tail was already read (it belongs
		// after this chunk's bytes and completes this chunk's last line). It is
		// memory-bounded: a line longer than maxLogLineSize cannot be carried
		// across many 8KB chunks (that would accumulate the whole line), so once
		// the carried remainder exceeds the limit the line is discarded
		// streaming - it is reported as incomplete, never materialized in full.
		data := append(chunk, remainder...)
		lines := splitLines(data)
		if chunkStart > 0 && len(lines) > 0 {
			if len(lines[0]) > maxLogLineSize {
				// The carried line is already bigger than the cap; drop it and
				// stop carrying so we do not keep accumulating it. Its bytes
				// still occupy their file positions, so the lines after it keep
				// their real offsets (the offset base below includes them).
				remainder = nil
				incomplete = true
				lines = lines[1:]
			} else {
				remainder = lines[0]
				lines = lines[1:]
			}
		} else {
			remainder = nil
		}

		// Walk this chunk's lines from newest to oldest, tracking each line's
		// byte offset (counting the newlines between lines) for the cursor. The
		// offset base is the end of this chunk's bytes (data = chunk + the
		// carried head), which is the correct absolute file position whether or
		// not the head was discarded.
		lineEnd := chunkStart + int64(len(data))
		if firstChunk {
			// On every later iteration, lineEnd's base is the absolute position
			// the previous iteration's carried remainder implicitly recorded,
			// which is correct by construction. The very first chunk of the
			// call has no such history: start is either the file size or a
			// cursor offset, and both land one byte past the newline that
			// terminates the newest line in scope - not on the newline itself -
			// unless the file/cursor boundary has no trailing newline (a file
			// with no final newline, or start == 0), in which case start is
			// already the correct boundary. Back lineEnd up by one only when
			// the read data actually ends in that newline, so the newest
			// line's derived lineStart lands one past it, not on it.
			if len(data) > 0 && data[len(data)-1] == '\n' {
				lineEnd--
			}
			firstChunk = false
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if len(line) == 0 {
				lineEnd-- // account for the newline separator
				continue
			}
			lineStart := lineEnd - int64(len(line))
			lineEnd = lineStart - 1 // include the separator newline in the gap

			// alreadyReturned is meant to guard against re-rendering the single
			// line the cursor points at (the previous page's oldest entry, at
			// offset == boundary). In practice it is provably unreachable: a
			// continuation reads chunks covering [chunkStart, pos) with pos
			// initialized to start (== boundary), so every lineStart derived
			// from bytes this scan ever reads is strictly less than boundary -
			// the boundary line itself sits just outside the read window by
			// construction, appends included. Verified empirically (a
			// two-call pagination walk never observes lineStart == boundary)
			// both before and after the firstChunk fix above, so this is
			// long-standing defensive dead code, not something the fix
			// revives. Left in place as a harmless safety net.
			alreadyReturned := boundary >= 0 && lineStart == boundary

			// parseRecord parses + classifies the line. ok is false on a skip
			// (oversized / malformed). It is the single place the parse happens
			// so the page and the count path agree on what "a match" is.
			parseRecord := func() (logRecord, bool) {
				if len(line) > maxLogLineSize {
					// This record is deliberately unclassified rather than held in
					// memory, so no count that skips it can claim exactness.
					incomplete = true
					return logRecord{}, false
				}
				var parsed map[string]any
				if err := json.Unmarshal(line, &parsed); err != nil {
					return logRecord{}, false
				}
				rec := logRecord{offset: lineStart, size: int64(len(line)) + 1, entry: parsed}
				rec.level = extractLevel(rec.entry)
				if t, ok := parseEntryTime(rec.entry); ok {
					rec.t, rec.hasTime = t, true
				}
				return rec, true
			}

			if len(page) >= limit {
				// The page is full; this line is older than it. Count it toward
				// matchedBehind (so match_count is the total) but do not render.
				// truncated is set only when this older line is itself a MATCH:
				// "more matches exist" is proven by seeing one, not merely by
				// the scan having stopped (block 2).
				if rec, ok := parseRecord(); ok && matchesFilter(filt, rec) {
					if filt.observe != nil {
						filt.observe(rec)
					}
					matchedBehind++
					truncated = true
				}
				continue
			}

			if alreadyReturned {
				// The previous page's oldest entry: skip without counting (it is
				// part of the already-returned page, not a new match).
				continue
			}
			rec, ok := parseRecord()
			if !ok {
				continue
			}
			if !matchesFilter(filt, rec) {
				continue
			}
			if filt.observe != nil {
				filt.observe(rec)
			}
			page = append(page, rec)
		}
		if len(page) > limit {
			// Safety: trim to the page.
			page = page[:limit]
		}
		pos = chunkStart
	}
	// The first line can remain in remainder after reaching byte zero. Mark an
	// oversized one incomplete before deciding count exactness.
	if len(remainder) > maxLogLineSize {
		incomplete = true
	}
	// reachedStart is true when the loop walked to the start of the file (pos
	// hit 0). Reaching byte zero means every byte was read, including when the
	// final read consumed the budget exactly. An oversized incomplete line is
	// still not exact because its contents could not be classified.
	if pos == 0 && !incomplete {
		reachedStart = true
	}

	// Final remainder: the very first line of the file, read once we reach the
	// start of the file. It is the oldest line in the file, so it is behind any
	// cursor and behind a full page; count it if it matches and the page is
	// full, render it only when the page is not yet full.
	if len(remainder) > 0 && len(remainder) <= maxLogLineSize {
		var entry map[string]any
		if err := json.Unmarshal(remainder, &entry); err == nil {
			rec := logRecord{offset: 0, size: int64(len(remainder)), entry: entry}
			rec.level = extractLevel(rec.entry)
			if t, ok := parseEntryTime(rec.entry); ok {
				rec.t, rec.hasTime = t, true
			}
			if matchesFilter(filt, rec) {
				if filt.observe != nil {
					filt.observe(rec)
				}
				if len(page) >= limit {
					matchedBehind++
					truncated = true
				} else {
					page = append(page, rec)
				}
			}
		}
	}

	// Reverse page from most-recent-first to chronological (oldest first).
	for i, j := 0, len(page)-1; i < j; i, j = i+1, j-1 {
		page[i], page[j] = page[j], page[i]
	}
	return backwardScanResult{entries: page, matchedBehind: matchedBehind, truncated: truncated, reachedStart: reachedStart}
}

// parseEntryTime parses an entry's "time" field to a time.Time. It reuses the
// same formats extractTime accepts (RFC3339 and the date-only variant) so the
// since filter and the display agree on what a timestamp means.
func parseEntryTime(entry map[string]any) (time.Time, bool) {
	v, ok := entry["time"].(string)
	if !ok {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02T15:04:05", v); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// matchesFilter reports whether rec satisfies the call's filter set. An entry
// with an unparseable/missing time is excluded when a since bound is set (we
// cannot prove it is new enough); otherwise it is kept.
func matchesFilter(f *logFilter, rec logRecord) bool {
	if f.level != "" && rec.level != f.level {
		return false
	}
	if f.component != "" {
		if c, _ := rec.entry["component"].(string); c != f.component {
			return false
		}
	}
	if f.sessionID != "" {
		if s, _ := rec.entry["session_id"].(string); s != f.sessionID {
			return false
		}
	}
	if f.runID != "" {
		if r, _ := rec.entry["run_id"].(string); r != f.runID {
			return false
		}
	}
	if f.sinceTime != nil {
		if !rec.hasTime || rec.t.Before(*f.sinceTime) {
			return false
		}
	}
	if f.contains != "" {
		if !containsMatch(rec.entry, f.contains) {
			return false
		}
	}
	if f.chain && !f.chainAnchors[extractMessage(rec.entry)] {
		return false
	}
	if f.accept != nil && !f.accept(rec.entry) {
		return false
	}
	return true
}

// containsMatch reports whether the case-insensitive needle appears in the
// entry's message or in any redaction-safe field value. Sensitive field values
// are deliberately NOT searched: contains must not become a way to exfiltrate
// a redacted secret (the filter could be satisfied by a value the output would
// redact).
func containsMatch(entry map[string]any, needle string) bool {
	if m := strings.ToLower(extractMessage(entry)); strings.Contains(m, needle) {
		return true
	}
	for k, v := range entry {
		if isReservedField(k) {
			continue
		}
		if isSensitiveKey(k) {
			continue // never search redacted values
		}
		if s, ok := v.(string); ok && strings.Contains(strings.ToLower(s), needle) {
			return true
		}
	}
	return false
}

// filterChainAnchors keeps only the diagnostic anchor entries from a chain
// scan, re-clamped to the chain cap (the backward scan already applied the
// anchor predicate; this trims to the cap and is the single place the chain's
// "minimal trace" size is decided).
func filterChainAnchors(recs []logRecord) []logRecord {
	out := make([]logRecord, 0, len(recs))
	for _, r := range recs {
		if !chainAnchorMsgs[extractMessage(r.entry)] {
			continue
		}
		out = append(out, r)
	}
	if len(out) > maxChainLines {
		out = out[:maxChainLines]
	}
	return out
}

// formatLogEntries renders records as the compact text lines plus the
// structured metadata footer and (in chain mode) the chain banner. It is the
// tool's full output; the backward-compatible per-line format is what the body
// uses, so line-based consumers that ignore the trailing metadata line are
// unaffected.
func formatLogEntries(entries []logRecord, meta SennitLogsResponseMetadata) string {
	lines := make([]string, 0, len(entries)+2)
	if meta.ChainMode {
		sess := meta.ChainSession
		run := meta.ChainRun
		if sess == "" {
			sess = "(all)"
		}
		if run == "" {
			run = "(all)"
		}
		lines = append(lines, fmt.Sprintf("correlated chain: session=%s run=%s (%d anchor lines)", sess, run, meta.ShownCount))
	}
	for _, rec := range entries {
		lines = append(lines, formatLogEntry(rec.entry))
	}
	lines = append(lines, formatMetaFooter(meta))
	return strings.Join(lines, "\n")
}

// formatMetaFooter renders the compact, human-readable metadata block that the
// structured SennitLogsResponseMetadata mirrors. It is one line so it does not
// disturb line-based consumers that ignore a trailing metadata line.
func formatMetaFooter(meta SennitLogsResponseMetadata) string {
	var b strings.Builder
	fmt.Fprintf(&b, "-- %d matched, %d shown", meta.MatchCount, meta.ShownCount)
	if meta.Truncated {
		b.WriteString(", truncated")
	}
	if meta.NextCursor != "" {
		fmt.Fprintf(&b, ", next_cursor=%s", meta.NextCursor)
	}
	return b.String()
}

// fileIdentity binds a cursor's byte offset to a file generation. A stable
// device/file ID is preferred. If it is unavailable, fingerprint records a
// deterministic SHA-256 digest over a fixed prefix plus fixed windows directly
// before and after the cursor anchor. Appending does not change these regions;
// replacing equal-size data with the same mtime does.
// fileIdentity binds a cursor's byte offset to a file generation. A stable
// device/file ID is preferred. When it is unavailable, the cursor stores the
// SHA-256 digest and length of the complete immutable prefix [0:offset].
// Verifying that prefix is deliberately O(offset) in this fallback path: it
// permits appends while detecting every change readable before the cursor.
type fileIdentity struct {
	dev          uint64
	ino          uint64
	prefixLength int64
	fingerprint  string
}

func newFileIdentity(file *os.File, info os.FileInfo, anchor int64) (fileIdentity, error) {
	return newFileIdentityWithStableID(file, info, anchor, fileDevInode)
}

// newFileIdentityWithStableID is a testable seam for platforms/filesystems
// that cannot supply a stable file ID.
func newFileIdentityWithStableID(file *os.File, info os.FileInfo, offset int64, stableID func(*os.File, os.FileInfo) (uint64, uint64)) (fileIdentity, error) {
	dev, ino := stableID(file, info)
	if ino != 0 {
		return fileIdentity{dev: dev, ino: ino}, nil
	}
	prefixLength := min(max(offset, 0), info.Size())
	fingerprint, err := cursorPrefixFingerprint(file, prefixLength)
	if err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{prefixLength: prefixLength, fingerprint: fingerprint}, nil
}

// cursorPrefixFingerprint hashes exactly [0:prefixLength], never bytes after
// the cursor offset. It streams the prefix in bounded chunks, but its total
// work is O(prefixLength), which is required to detect arbitrary changes in a
// stable-ID-less file.
func cursorPrefixFingerprint(file *os.File, prefixLength int64) (string, error) {
	if prefixLength < 0 {
		return "", fmt.Errorf("negative cursor prefix length")
	}
	h := sha256.New()
	buf := make([]byte, 32*1024)
	for offset := int64(0); offset < prefixLength; {
		want := min(int64(len(buf)), prefixLength-offset)
		n, err := file.ReadAt(buf[:want], offset)
		if err != nil && err != io.EOF {
			return "", err
		}
		if int64(n) != want {
			return "", io.ErrUnexpectedEOF
		}
		_, _ = h.Write(buf[:n])
		offset += int64(n)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (id fileIdentity) matches(file *os.File, info os.FileInfo, anchor int64) (bool, error) {
	return id.matchesWithStableID(file, info, anchor, fileDevInode)
}

func (id fileIdentity) matchesWithStableID(file *os.File, info os.FileInfo, offset int64, stableID func(*os.File, os.FileInfo) (uint64, uint64)) (bool, error) {
	if id.ino != 0 {
		dev, ino := stableID(file, info)
		return id.dev == dev && id.ino == ino, nil
	}
	if id.prefixLength != offset || id.prefixLength < 0 || id.prefixLength > info.Size() || id.fingerprint == "" {
		return false, nil // malformed or incompatible fallback cursor fails closed
	}
	fingerprint, err := cursorPrefixFingerprint(file, id.prefixLength)
	if err != nil {
		return false, err
	}
	return id.fingerprint == fingerprint, nil
}

// cursorPrefix identifies the cursor format. It is bumped whenever the
// payload changes shape so a cursor minted by an older build is rejected
// (treated as stale) rather than mis-decoded.
const cursorPrefix = "slc4"

// encodeCursor packs a continuation (offset, running match count, and the file
// generation identity) into an opaque single token. The base64 makes it
// obvious the cursor is not a line number or offset a caller should
// hand-compute, while staying a stable function of its inputs alone.
func encodeCursor(offset, returned int64, id fileIdentity) string {
	if offset < 0 {
		offset = 0
	}
	if returned < 0 {
		returned = 0
	}
	return cursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(
		"%d:%d:%d:%d:%d:%s", offset, returned, id.dev, id.ino, id.prefixLength, id.fingerprint)))
}

// decodeCursor parses a cursor minted by encodeCursor. It returns the decoded
// offset/returned and the recorded file identity. The offset is clamped to
// [0, fileSize] so a cursor that points past the current end of the file
// (rotation that shrank it) reads empty rather than erroring - the documented
// rotation behavior. Malformed or wrong-version cursors are an error (the
// caller reports "stale cursor" and starts over).
func decodeCursor(cursor string) (offset, returned int64, id fileIdentity, err error) {
	if !strings.HasPrefix(cursor, cursorPrefix) {
		return 0, 0, fileIdentity{}, fmt.Errorf("not a sennit_logs cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor[len(cursorPrefix):])
	if err != nil {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor: %w", err)
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 6 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor: wrong field count")
	}
	parse := func(i int) (int64, error) { return strconv.ParseInt(parts[i], 10, 64) }
	if offset, err = parse(0); err != nil || offset < 0 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor offset")
	}
	if returned, err = parse(1); err != nil || returned < 0 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor returned count")
	}
	dev, err := parse(2)
	if err != nil || dev < 0 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor device")
	}
	ino, err := parse(3)
	if err != nil || ino < 0 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor inode")
	}
	prefixLength, err := parse(4)
	if err != nil || prefixLength < 0 {
		return 0, 0, fileIdentity{}, fmt.Errorf("undecodable cursor prefix length")
	}
	id = fileIdentity{dev: uint64(dev), ino: uint64(ino), prefixLength: prefixLength, fingerprint: parts[5]}
	if id.ino == 0 {
		if id.prefixLength != offset || len(id.fingerprint) != sha256.Size*2 {
			return 0, 0, fileIdentity{}, fmt.Errorf("cursor lacks a valid file prefix")
		}
		if _, err := hex.DecodeString(id.fingerprint); err != nil {
			return 0, 0, fileIdentity{}, fmt.Errorf("cursor has an invalid file fingerprint")
		}
	}
	return offset, returned, id, nil
}

// splitLines splits data into lines without allocating strings.
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := range len(data) {
		if data[i] == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// formatLogEntry formats a single log entry into compact text format:
// TIMESTAMP LEVEL SOURCE:LINE MESSAGE key=value...
func formatLogEntry(entry map[string]any) string {
	var parts []string

	// Extract and format timestamp (time-only, no date).
	timeStr := extractTime(entry)
	parts = append(parts, timeStr)

	// Extract level.
	level := extractLevel(entry)
	parts = append(parts, level)

	// Extract source.
	source := extractSource(entry)
	parts = append(parts, source)

	// Extract message.
	msg := extractMessage(entry)

	// Collect extra fields (excluding reserved fields).
	extraFields := extractExtraFields(entry)

	// Build the output.
	var b strings.Builder
	for i, part := range parts {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	b.WriteByte(' ')
	b.WriteString(msg)

	// Append sorted key=value pairs.
	if len(extraFields) > 0 {
		keys := make([]string, 0, len(extraFields))
		for k := range extraFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			b.WriteByte(' ')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(formatValue(extraFields[k], k))
		}
	}

	return b.String()
}

// extractTime extracts and formats the timestamp from a log entry.
// Returns time-only format (15:04:05).
func extractTime(entry map[string]any) string {
	timeVal, ok := entry["time"]
	if !ok {
		return "--:--:--"
	}

	timeStr, ok := timeVal.(string)
	if !ok {
		return "--:--:--"
	}

	// Parse RFC3339 format.
	t, err := time.Parse(time.RFC3339, timeStr)
	if err != nil {
		// Try other common formats.
		t, err = time.Parse("2006-01-02T15:04:05", timeStr)
		if err != nil {
			return "--:--:--"
		}
	}

	return t.Format("15:04:05")
}

// extractLevel extracts and normalizes the log level.
func extractLevel(entry map[string]any) string {
	levelVal, ok := entry["level"]
	if !ok {
		return "INFO"
	}

	levelStr, ok := levelVal.(string)
	if !ok {
		return "INFO"
	}

	switch strings.ToUpper(levelStr) {
	case "DEBUG":
		return "DEBUG"
	case "INFO":
		return "INFO"
	case "WARN", "WARNING":
		return "WARN"
	case "ERROR":
		return "ERROR"
	default:
		return "INFO"
	}
}

// extractSource extracts the source file and line from a log entry.
func extractSource(entry map[string]any) string {
	sourceVal, ok := entry["source"]
	if !ok {
		return "unknown:0"
	}

	// Source can be a string or an object with "file" and "line".
	switch s := sourceVal.(type) {
	case string:
		return filepath.Base(s)
	case map[string]any:
		fileVal, ok := s["file"].(string)
		if !ok {
			return "unknown:0"
		}
		fileVal = filepath.Base(fileVal)

		lineNum := 0
		if lineVal, ok := s["line"]; ok {
			switch l := lineVal.(type) {
			case float64:
				lineNum = int(l)
			case int:
				lineNum = l
			case json.Number:
				if n, err := l.Int64(); err == nil {
					lineNum = int(n)
				}
			}
		}
		return fmt.Sprintf("%s:%d", fileVal, lineNum)
	default:
		return "unknown:0"
	}
}

// extractMessage extracts the log message.
func extractMessage(entry map[string]any) string {
	msgVal, ok := entry["msg"]
	if !ok {
		return ""
	}

	if msgStr, ok := msgVal.(string); ok {
		return msgStr
	}

	return fmt.Sprintf("%v", msgVal)
}

// extractExtraFields extracts all non-reserved fields from a log entry.
func extractExtraFields(entry map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range entry {
		// Skip reserved fields (case-insensitive).
		if isReservedField(k) {
			continue
		}
		// Redact sensitive values.
		if isSensitiveKey(k) {
			result[k] = "[REDACTED]"
		} else {
			result[k] = v
		}
	}
	return result
}

// isReservedField checks if a field name is reserved (case-insensitive).
func isReservedField(name string) bool {
	lowerName := strings.ToLower(name)
	return reservedFields[lowerName]
}

// isSensitiveKey checks if a key contains sensitive information (case-insensitive).
func isSensitiveKey(name string) bool {
	lowerName := strings.ToLower(name)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(lowerName, sensitive) {
			return true
		}
	}
	return false
}

// formatValue formats a value according to the quoting rules.
func formatValue(value any, key string) string {
	// Redact sensitive values (second check for safety).
	if isSensitiveKey(key) {
		return "[REDACTED]"
	}

	switch v := value.(type) {
	case string:
		return formatStringValue(v)
	case float64:
		// Check if it's actually an integer.
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	case map[string]any, []any:
		// Objects and arrays are JSON-encoded and quoted.
		jsonBytes, err := json.Marshal(v)
		if err != nil {
			return quoteString(fmt.Sprintf("%v", v))
		}
		return quoteString(string(jsonBytes))
	default:
		return quoteString(fmt.Sprintf("%v", v))
	}
}

// formatStringValue formats a string value with quoting if needed.
func formatStringValue(s string) string {
	// Quote if empty, contains spaces, =, newlines, or special chars.
	needsQuote := len(s) == 0 ||
		strings.ContainsAny(s, " =\n\r\t\"") ||
		strings.Contains(s, "\\")

	if !needsQuote {
		return s
	}

	return quoteString(s)
}

// quoteString quotes a string with double quotes and escapes special characters.
func quoteString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

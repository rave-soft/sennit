package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The helpers here collect a whole result into a slice. Production never
// does: grep, git diff --stat and read all page their output, so the
// collecting form has no caller outside tests and used to sit in the
// production files next to the streaming form it was replaced by — where
// it read as a second, simpler way to do the same thing rather than as
// test scaffolding.

// searchWithRipgrep collects every match for a case-sensitive search.
// Production goes through visitRipgrepMatches, which stops at a page.
func searchWithRipgrep(ctx context.Context, pattern, path, include string) ([]grepMatch, error) {
	return searchWithRipgrepCommand(ctx, pattern, path, include, false, getRgSearchCmd)
}

func searchWithRipgrepCommand(ctx context.Context, pattern, path, include string, caseInsensitive bool, command func(context.Context, string, string, string, bool) *exec.Cmd) ([]grepMatch, error) {
	var matches []grepMatch
	err := visitRipgrepMatches(ctx, pattern, path, include, caseInsensitive, command, func(match grepMatch) {
		matches = append(matches, match)
	})
	return matches, err
}

// readTextFile drops the line count readTextFileCount also returns, which
// every production caller uses.
func readTextFile(filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	content, hasMore, _, err := readTextFileCount(filePath, offset, limit, maxContentSize)
	return content, hasMore, err
}

// parseNumstat slurps a whole `git diff --numstat -z` payload. Production
// reads the same format through visitNumstat, one entry at a time, so that
// a large diff never has to be held in memory at once.
func parseNumstat(data []byte) []gitDiffStatEntry {
	fields := bytes.Split(data, []byte{0})
	var out []gitDiffStatEntry
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(string(fields[i]), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		e := gitDiffStatEntry{Added: parts[0], Deleted: parts[1], Path: parts[2]}
		if e.Path == "" && i+2 < len(fields) {
			e.OriginalPath, e.Path = string(fields[i+1]), string(fields[i+2])
			i += 2
		}
		out = append(out, e)
	}
	return out
}

// The four below are the pre-T5 shapes of the sennit_logs entry points,
// kept because the tests written against that signature still are. They
// were documented in the production file as being for "the pre-T5 tests
// and any text-only consumer" — the consumer never arrived, and a helper
// whose stated audience is a test is scaffolding wherever it is written.

// runSennitLogsText returns only the formatted entry lines: no structured
// metadata, no error, no footer.
func runSennitLogsText(logFile string, params SennitLogsParams) string {
	output, _, err := runSennitLogs(logFile, params)
	if err != nil {
		return err.Error()
	}
	return stripMetaFooter(output)
}

// stripMetaFooter removes the trailing "-- ..." metadata footer line that
// runSennitLogs appends.
func stripMetaFooter(output string) string {
	idx := strings.LastIndex(output, "\n-- ")
	if idx < 0 {
		return output
	}
	return strings.TrimRight(output[:idx], "\n")
}

// readLastLines reads the most recent n valid entries from the end of the
// file in chronological order. The 8KB chunk-boundary carrying it relies on
// is pinned by TestReadLastLinesKeepsEntriesAcrossChunkBoundaries.
func readLastLines(filePath string, n int) ([]map[string]any, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() == 0 {
		return nil, nil
	}
	res := scanBackward(file, stat.Size(), -1, mustLogFilter(), n)
	entries := make([]map[string]any, 0, len(res.entries))
	for _, rec := range res.entries {
		entries = append(entries, rec.entry)
	}
	return entries, nil
}

// mustLogFilter builds a filter for the zero params (no level/since), so it
// cannot fail.
func mustLogFilter() *logFilter {
	f, err := newLogFilter(SennitLogsParams{})
	if err != nil {
		// Unreachable: the zero params have no level/since to validate.
		panic(fmt.Sprintf("mustLogFilter: %v", err))
	}
	return f
}

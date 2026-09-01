package message

import "strings"

// DelegationReportPrefix opens every report a delegation writes into the
// session that started it: a terminal completion, or a mid-run ask.
//
// It is here, in the leaf package both sides already import, because two
// packages need the same string for different reasons. internal/agent
// writes it so the model does not read the report as something the
// person typed (the report is persisted as a user-role message with
// Origin agent — see runTurn.foldCompletions). internal/ui reads it back
// to recognize a report in the transcript, where it is the one
// user-role message nobody wrote and nobody wants opened by default: a
// delegation's final answer is as long as the work it did.
const DelegationReportPrefix = "[system-generated delegation report - not user input]"

// IsDelegationReport reports whether m is one of those reports.
//
// All three conditions are load-bearing: the role and origin are what
// the store records, and the prefix is what separates a report from a
// delegation's own goal, which is persisted with the same role and
// origin and is ordinary text the reader does want to see.
func IsDelegationReport(m *Message) bool {
	if m == nil || m.Role != User || m.Origin != OriginAgent {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(m.Content().Text), DelegationReportPrefix)
}

// DelegationReportField returns the value of a "key: value" line in a
// report's header block, or "" when the report has no such line.
//
// The header is written by formatTaskCompletion in internal/agent, a
// handful of fixed fields on their own lines. Reading it back by name is
// less brittle than counting lines, and a missing field simply leaves
// that part of the collapsed row empty rather than shifting everything
// after it.
func DelegationReportField(text, key string) string {
	prefix := key + ":"
	for line := range strings.SplitSeq(text, "\n") {
		// The body below the header can contain anything, including a
		// line that looks like a field. Stop at the blank line that
		// separates the header from the result.
		if strings.TrimSpace(line) == "" {
			return ""
		}
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

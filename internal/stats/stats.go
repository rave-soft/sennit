// Package stats aggregates recorded usage — tokens, cost, wall time, and
// how background delegations ended — into the breakdowns both `sennit
// stat` and the TUI's /stats screen render.
//
// It exists as its own package because those two surfaces need the same
// numbers under different scopes: the command has always reported one
// project over a time window, while the screen also has to answer "what
// did this session cost" and "what has everything cost". Keeping the
// aggregation here means the two can never drift into disagreeing about
// the same data.
//
// # What the data can and cannot say
//
// Two limits are inherent to what the database records, not to this
// package, and both are surfaced rather than hidden:
//
// Token counts are stored per session, never per message. A session that
// used a single model throughout attributes its tokens and cost to that
// model exactly. A session that switched models has its totals split
// across them in proportion to each model's share of the session's
// assistant messages, and every row built that way is marked
// [Model.Approximate]. Message counts and time are always exact — they
// come from per-message timestamps.
//
// A delegation's outcome is its own terminal status: completed or merged
// versus failed or cancelled. Whether a reviewer approved the work is not
// something this process records, so [Outcome] counts what landed, not
// what passed review.
package stats

import (
	"math"
	"slices"
	"sort"
	"strings"
)

// Session is one session's contribution to a breakdown, in the shape the
// aggregation needs. The db package generates a distinct row type per
// query, so callers convert into this at the query seam and everything
// below works the same for all three scopes.
type Session struct {
	ID       string
	ParentID string
	Title    string
	// AgentID is the delegating agent's id, stamped on sub-agent sessions
	// since the agent_id column was added. It is empty for top-level
	// sessions and for delegations created before that column existed —
	// see [Agent.Name] for what happens then.
	AgentID          string
	PromptTokens     int64
	CompletionTokens int64
	Cost             float64
	CreatedAt        int64
	UpdatedAt        int64
}

// IsSubAgent reports whether s was created by a delegation rather than by
// a person starting a session.
func (s Session) IsSubAgent() bool { return s.ParentID != "" }

// Message is one assistant message, the unit that carries which model
// actually answered and how long it took.
type Message struct {
	SessionID  string
	Model      string
	Provider   string
	CreatedAt  int64
	FinishedAt int64
}

// Delegation is one background task or thread and how it ended.
type Delegation struct {
	ID     string
	Kind   string
	Status string
	// SessionID is the session that ran the delegation; AgentID and Title
	// are that session's, carried along so an outcome can be attributed
	// without a second lookup.
	SessionID   string
	AgentID     string
	Title       string
	CreatedAt   int64
	CompletedAt int64
}

// ModelKey identifies a (model, provider) pair. The same model id can be
// served by more than one provider, and their costs differ, so the pair
// is the identity — not the model id alone.
type ModelKey struct {
	Model    string
	Provider string
}

// Model is one row of the per-model breakdown.
type Model struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider"`
	MessageCount     int64   `json:"message_count"`
	TimeSeconds      int64   `json:"time_seconds"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	// Approximate is true when this row's tokens/cost were split
	// proportionally across the models of a mixed-model session rather
	// than attributed from a single-model session exactly. Renderers mark
	// such rows so a reader does not take an estimate for a measurement.
	Approximate bool `json:"approximate"`
	// Delegations counts background delegations whose session ran on this
	// model, and Succeeded how many of those ended in a landed state.
	Delegations int64 `json:"delegations"`
	Succeeded   int64 `json:"succeeded"`
}

// Tokens is the row's total token count, the figure both the sort order
// and the bar widths are built on.
func (m Model) Tokens() int64 { return m.PromptTokens + m.CompletionTokens }

// Agent is one row of the per-subagent breakdown. Unlike model
// attribution this is exact: every sub-agent session carries its own
// tokens, cost, and wall-clock duration.
type Agent struct {
	Name             string  `json:"name"`
	Runs             int64   `json:"runs"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	TimeSeconds      int64   `json:"time_seconds"`
	// Delegations counts this agent's background delegations and
	// Succeeded how many landed. Both are zero for an agent that only
	// ever ran inline, which is the common case: most sub-agent runs are
	// not delegations.
	Delegations int64 `json:"delegations"`
	Succeeded   int64 `json:"succeeded"`
}

// Tokens is the row's total token count.
func (a Agent) Tokens() int64 { return a.PromptTokens + a.CompletionTokens }

// Project is a totals row: one project, or a whole scope's summary.
type Project struct {
	Path             string  `json:"path"`
	Sessions         int64   `json:"sessions"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Cost             float64 `json:"cost"`
	TimeSeconds      int64   `json:"time_seconds"`
}

// Tokens is the row's total token count.
func (p Project) Tokens() int64 { return p.PromptTokens + p.CompletionTokens }

// Skill is one row of the skill-usage breakdown.
type Skill struct {
	Name         string `json:"name"`
	LoadCount    int64  `json:"load_count"`
	SessionCount int64  `json:"session_count"`
	FirstUsedAt  string `json:"first_used_at,omitempty"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

// LatencyEvent is one recorded internal handoff wait, in the shape the
// aggregation needs. Kind is one of the values internal/latency writes;
// this package deliberately does not enumerate them, so a kind added
// there shows up here as its own row without a change on this side.
type LatencyEvent struct {
	Kind     string
	WaitedMS int64
}

// Latency is one row of the per-kind latency breakdown: how long that
// handoff waited across the scope, as a distribution rather than a mean.
//
// Percentiles, not an average, because the failure this is meant to
// catch is a tail — a handoff that is usually instant and occasionally
// stalls averages out to "fine". P50 answers "what does this normally
// cost", P95 and Max answer "how bad does it get".
type Latency struct {
	Kind   string `json:"kind"`
	Events int64  `json:"events"`
	P50MS  int64  `json:"p50_ms"`
	P95MS  int64  `json:"p95_ms"`
	MaxMS  int64  `json:"max_ms"`
}

// Outcome summarizes how background delegations ended within a scope.
// Landed counts the ones that reached a completed or merged state;
// Failed counts failures and cancellations. Statuses this build does not
// recognize (a newer overlay sharing the database) fall into neither, so
// an unknown state is never silently scored as a success.
type Outcome struct {
	Total  int64 `json:"total"`
	Landed int64 `json:"landed"`
	Failed int64 `json:"failed"`
}

// Rate is the share of delegations that landed, in [0,1]. It is computed
// over Total rather than Landed+Failed so unrecognized and still-running
// states dilute the rate instead of being excluded from it — a run that
// has not landed has not landed.
func (o Outcome) Rate() float64 {
	if o.Total == 0 {
		return 0
	}
	return float64(o.Landed) / float64(o.Total)
}

// landedStatuses and failedStatuses name the delegation statuses that
// count as having landed or failed. They mirror internal/thread's Status
// values but are spelled out here as plain strings: this package reads
// rows out of the database, including rows written by builds whose status
// set differs from this one's, so matching on text is the honest model.
var (
	landedStatuses = map[string]bool{"completed": true, "merged": true}
	failedStatuses = map[string]bool{"failed": true, "cancelled": true}
)

// Snapshot is everything one scope's aggregation produced.
type Snapshot struct {
	Totals   Project   `json:"totals"`
	Models   []Model   `json:"models,omitempty"`
	Agents   []Agent   `json:"agents,omitempty"`
	Projects []Project `json:"projects,omitempty"`
	Skills   []Skill   `json:"skills,omitempty"`
	Latency  []Latency `json:"latency,omitempty"`
	Outcome  Outcome   `json:"outcome"`
}

// Empty reports whether the snapshot found nothing at all to report, so a
// renderer can say "no usage recorded yet" instead of drawing a frame
// around zeroes.
func (s Snapshot) Empty() bool {
	return s.Totals.Sessions == 0 && len(s.Models) == 0 && len(s.Agents) == 0 && s.Outcome.Total == 0
}

// ComputeModels groups assistant messages by (model, provider) for exact
// message counts and time, then attributes each session's tokens and cost
// to the model(s) its assistant messages used — exactly for single-model
// sessions, proportionally (and marked Approximate) for sessions that
// mixed models. delegations, when non-nil, additionally attributes each
// delegation's outcome to the model its session ran on.
func ComputeModels(sessions []Session, messages []Message, delegations []Delegation) []Model {
	messagesBySession := make(map[string][]Message)
	timeByModel := make(map[ModelKey]int64)
	countByModel := make(map[ModelKey]int64)
	for _, m := range messages {
		key := ModelKey{Model: m.Model, Provider: m.Provider}
		timeByModel[key] += m.FinishedAt - m.CreatedAt
		countByModel[key]++
		messagesBySession[m.SessionID] = append(messagesBySession[m.SessionID], m)
	}

	type tokenTotals struct {
		prompt     int64
		completion int64
		cost       float64
	}
	tokensByModel := make(map[ModelKey]tokenTotals)
	approxByModel := make(map[ModelKey]bool)

	for _, s := range sessions {
		msgs := messagesBySession[s.ID]
		if len(msgs) == 0 {
			continue
		}
		countPerModel := make(map[ModelKey]int64)
		for _, m := range msgs {
			countPerModel[ModelKey{Model: m.Model, Provider: m.Provider}]++
		}

		if len(countPerModel) == 1 {
			for key := range countPerModel {
				t := tokensByModel[key]
				t.prompt += s.PromptTokens
				t.completion += s.CompletionTokens
				t.cost += s.Cost
				tokensByModel[key] = t
			}
			continue
		}

		var total int64
		for _, c := range countPerModel {
			total += c
		}
		for key, c := range countPerModel {
			share := float64(c) / float64(total)
			t := tokensByModel[key]
			t.prompt += int64(math.Round(float64(s.PromptTokens) * share))
			t.completion += int64(math.Round(float64(s.CompletionTokens) * share))
			t.cost += s.Cost * share
			tokensByModel[key] = t
			approxByModel[key] = true
		}
	}

	// A delegation is attributed to the model that answered most of its
	// session's assistant messages. Splitting an outcome proportionally
	// the way tokens are split would be meaningless — a task either landed
	// or it did not, and half a success is not a thing — so the dominant
	// model takes the whole row.
	delegationsByModel := make(map[ModelKey]*Outcome)
	for _, d := range delegations {
		key, ok := dominantModel(messagesBySession[d.SessionID])
		if !ok {
			continue
		}
		o := delegationsByModel[key]
		if o == nil {
			o = &Outcome{}
			delegationsByModel[key] = o
		}
		o.count(d.Status)
	}

	keys := make(map[ModelKey]bool)
	for k := range countByModel {
		keys[k] = true
	}
	for k := range tokensByModel {
		keys[k] = true
	}

	result := make([]Model, 0, len(keys))
	for k := range keys {
		t := tokensByModel[k]
		row := Model{
			Model:            k.Model,
			Provider:         k.Provider,
			MessageCount:     countByModel[k],
			TimeSeconds:      timeByModel[k],
			PromptTokens:     t.prompt,
			CompletionTokens: t.completion,
			Cost:             t.cost,
			Approximate:      approxByModel[k],
		}
		if o := delegationsByModel[k]; o != nil {
			row.Delegations = o.Total
			row.Succeeded = o.Landed
		}
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		if ti, tj := result[i].Tokens(), result[j].Tokens(); ti != tj {
			return ti > tj
		}
		return result[i].Model < result[j].Model
	})
	return result
}

// dominantModel returns the (model, provider) that answered the most of
// msgs, and false when there is nothing to go on. Ties break on the model
// id so the answer does not depend on map iteration order.
func dominantModel(msgs []Message) (ModelKey, bool) {
	if len(msgs) == 0 {
		return ModelKey{}, false
	}
	counts := make(map[ModelKey]int64, 2)
	for _, m := range msgs {
		counts[ModelKey{Model: m.Model, Provider: m.Provider}]++
	}
	var best ModelKey
	var bestCount int64
	for k, c := range counts {
		if c > bestCount || (c == bestCount && k.Model < best.Model) {
			best, bestCount = k, c
		}
	}
	return best, true
}

// count folds one delegation status into the outcome.
func (o *Outcome) count(status string) {
	o.Total++
	switch {
	case landedStatuses[status]:
		o.Landed++
	case failedStatuses[status]:
		o.Failed++
	}
}

// ComputeAgents groups sub-agent sessions by the agent that ran them.
func ComputeAgents(sessions []Session, delegations []Delegation) []Agent {
	byName := make(map[string]*Agent)
	var order []string
	agentOfSession := make(map[string]string, len(sessions))
	for _, s := range sessions {
		if !s.IsSubAgent() {
			continue
		}
		name := AgentName(s)
		agentOfSession[s.ID] = name
		a, ok := byName[name]
		if !ok {
			a = &Agent{Name: name}
			byName[name] = a
			order = append(order, name)
		}
		a.Runs++
		a.PromptTokens += s.PromptTokens
		a.CompletionTokens += s.CompletionTokens
		a.Cost += s.Cost
		a.TimeSeconds += s.UpdatedAt - s.CreatedAt
	}

	for _, d := range delegations {
		name := agentOfSession[d.SessionID]
		if name == "" {
			// A delegation whose session is outside this scope (or was
			// deleted) still has its own recorded agent id/title to fall
			// back on; only when both are empty is there no row to put it
			// on.
			name = delegationAgentName(d)
		}
		if name == "" {
			continue
		}
		a, ok := byName[name]
		if !ok {
			a = &Agent{Name: name}
			byName[name] = a
			order = append(order, name)
		}
		o := Outcome{Total: a.Delegations, Landed: a.Succeeded}
		o.count(d.Status)
		a.Delegations, a.Succeeded = o.Total, o.Landed
	}

	result := make([]Agent, 0, len(order))
	for _, name := range order {
		result = append(result, *byName[name])
	}
	sort.Slice(result, func(i, j int) bool {
		if ti, tj := result[i].Tokens(), result[j].Tokens(); ti != tj {
			return ti > tj
		}
		return result[i].Name < result[j].Name
	})
	return result
}

// AgentName is what a sub-agent session is grouped under: its agent id
// when it has one, and otherwise its title.
//
// The title fallback is why this is a function and not a field read. The
// agent_id column is recent; sessions recorded before it exists carry
// only a title, and delegations dispatched through the generic task tool
// carry no agent id at all. Grouping those by title keeps them visible as
// distinct rows instead of collapsing every one of them into a single
// nameless bucket — which is what the CLI's title-only grouping used to
// do to *every* custom agent.
func AgentName(s Session) string {
	return agentNameFrom(s.AgentID, s.Title)
}

// delegationAgentName is AgentName for a delegation row whose session did
// not come back with the scope.
func delegationAgentName(d Delegation) string {
	return agentNameFrom(d.AgentID, d.Title)
}

// agentNameFrom is the shared grouping rule behind [AgentName] and
// [delegationAgentName]: prefer the agent id, and fall back to the title
// when it's empty. Both callers read the same two columns off different
// structs (a session vs. a delegation row), so the logic lives here once.
func agentNameFrom(agentID, title string) string {
	if name := strings.TrimSpace(agentID); name != "" {
		return name
	}
	return strings.TrimSpace(title)
}

// ComputeTotals aggregates top-level sessions (those a person started)
// into a single row. Sub-agent sessions are deliberately excluded: their
// cost is already folded into their parent's, so counting both would
// double it.
func ComputeTotals(sessions []Session) Project {
	var p Project
	for _, s := range sessions {
		if s.IsSubAgent() {
			continue
		}
		p.Sessions++
		p.PromptTokens += s.PromptTokens
		p.CompletionTokens += s.CompletionTokens
		p.Cost += s.Cost
		p.TimeSeconds += s.UpdatedAt - s.CreatedAt
	}
	return p
}

// ComputeOutcome folds every delegation in a scope into one summary.
func ComputeOutcome(delegations []Delegation) Outcome {
	var o Outcome
	for _, d := range delegations {
		o.count(d.Status)
	}
	return o
}

// ComputeLatency groups recorded waits by kind and reduces each group to
// a count and three points of its distribution. Rows are ordered by kind
// so the table is stable between runs — unlike the token breakdowns
// there is no "biggest" to sort by, and sorting by P95 would make the
// rows swap places precisely when a reader is comparing two runs.
func ComputeLatency(events []LatencyEvent) []Latency {
	byKind := make(map[string][]int64)
	for _, e := range events {
		byKind[e.Kind] = append(byKind[e.Kind], e.WaitedMS)
	}

	rows := make([]Latency, 0, len(byKind))
	for kind, waits := range byKind {
		slices.Sort(waits)
		rows = append(rows, Latency{
			Kind:   kind,
			Events: int64(len(waits)),
			P50MS:  percentile(waits, 0.50),
			P95MS:  percentile(waits, 0.95),
			MaxMS:  waits[len(waits)-1],
		})
	}
	slices.SortFunc(rows, func(a, b Latency) int { return strings.Compare(a.Kind, b.Kind) })
	return rows
}

// percentile returns the nearest-rank percentile of an ascending slice:
// the smallest recorded value at or above the p-th position. Nearest
// rank rather than interpolation because every value here is a real
// observed wait, and reporting a wait that never happened would invite
// exactly the "but nothing took 87ms" confusion this table exists to
// avoid. sorted must be non-empty.
func percentile(sorted []int64, p float64) int64 {
	rank := min(max(int(math.Ceil(p*float64(len(sorted)))), 1), len(sorted))
	return sorted[rank-1]
}

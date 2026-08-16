package dialog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/dustin/go-humanize"
	"github.com/rave-soft/sennit/internal/stats"
	"github.com/rave-soft/sennit/internal/ui/common"
	"github.com/rave-soft/sennit/internal/ui/styles"
)

const (
	// StatsID is the identifier for the usage statistics dialog.
	StatsID             = "stats"
	statsDialogMaxWidth = 84
	// statsRowsPerSection caps how many models/agents one section lists.
	// The interesting shape of this data is always at the top — the few
	// models and agents doing most of the work — and an uncapped list
	// would push the sections below it off screen.
	statsRowsPerSection = 8
	// statsBarWidth is the width of the share bar drawn beside each row.
	statsBarWidth = 12
)

// statsTab is one scope the dialog can show. The order here is the order
// the tabs appear in, narrowest first: what this session cost, what this
// project has cost, what everything has cost.
type statsTab struct {
	scope stats.Scope
	label string
}

var statsTabs = []statsTab{
	{stats.ScopeSession, "Session"},
	{stats.ScopeProject, "Project"},
	{stats.ScopeGlobal, "All projects"},
}

// StatsLoadedMsg carries one tab's finished aggregation back to the
// dialog. Gathering runs off the render loop because the global scope
// sweeps every message row in the window, which is not work to do inside
// a Draw call.
type StatsLoadedMsg struct {
	Scope    stats.Scope
	Snapshot stats.Snapshot
	Err      error
}

// Stats is the /stats screen: recorded token, cost, time, and delegation
// usage, in three scopes selected by tabs.
//
// Two things it reports need reading carefully, and the footer says so
// rather than leaving the numbers to imply more precision than they have.
// Token counts are stored per session, so a session that switched models
// has its tokens split proportionally and its rows marked "~". And a
// delegation counts as landed when it finished or merged — nothing here
// knows whether a reviewer approved the work, only whether the run
// reached a completed state.
type Stats struct {
	Base
	com       *common.Common
	sessionID string

	active int
	// snapshots, errs, and loading are keyed by tab index rather than
	// scope so a tab is exactly one cell in each: the dialog keeps every
	// tab it has loaded, so switching back is instant and does not
	// re-query.
	snapshots map[int]stats.Snapshot
	errs      map[int]error
	loading   map[int]bool

	help   help.Model
	keyMap struct {
		Next  key.Binding
		Prev  key.Binding
		Close key.Binding
	}
}

var _ Dialog = (*Stats)(nil)

// NewStats creates the /stats dialog. sessionID is the session the
// Session tab reports on; when it is empty that tab reports nothing to
// show rather than silently falling back to another scope, since "this
// session" with no session is a question with no answer.
func NewStats(com *common.Common, sessionID string) *Stats {
	d := &Stats{
		Base:      NewBase(com, statsDialogMaxWidth),
		com:       com,
		sessionID: sessionID,
		snapshots: make(map[int]stats.Snapshot, len(statsTabs)),
		errs:      make(map[int]error, len(statsTabs)),
		loading:   make(map[int]bool, len(statsTabs)),
	}
	// Open on Project: a session's own numbers are already on the
	// sidebar, so the first thing this screen can say that nothing else
	// does is what the project as a whole has cost.
	d.active = 1

	h := help.New()
	h.Styles = com.Styles.DialogHelpStyles()
	d.help = h

	d.keyMap.Next = key.NewBinding(
		key.WithKeys("tab", "right", "l"),
		key.WithHelp("tab/→", "next tab"),
	)
	d.keyMap.Prev = key.NewBinding(
		key.WithKeys("shift+tab", "left", "h"),
		key.WithHelp("shift+tab/←", "prev tab"),
	)
	d.keyMap.Close = CloseKey

	return d
}

// ID implements [Dialog].
func (d *Stats) ID() string { return StatsID }

// LoadCmd gathers the active tab's snapshot off the render loop. The
// dialog's opener calls this once; tab switches produce their own through
// HandleMsg.
func (d *Stats) LoadCmd() tea.Cmd {
	return d.loadTab(d.active)
}

// loadTab returns a command gathering tab i, or nil when that tab is
// already loaded, already in flight, or cannot be loaded at all.
func (d *Stats) loadTab(i int) tea.Cmd {
	if i < 0 || i >= len(statsTabs) {
		return nil
	}
	if _, ok := d.snapshots[i]; ok {
		return nil
	}
	if d.loading[i] {
		return nil
	}
	scope := statsTabs[i].scope
	if scope == stats.ScopeSession && d.sessionID == "" {
		return nil
	}
	if d.com == nil || d.com.Workspace == nil {
		return nil
	}

	d.loading[i] = true
	ws := d.com.Workspace
	req := stats.Request{Scope: scope}
	switch scope {
	case stats.ScopeSession:
		req.SessionID = d.sessionID
	case stats.ScopeProject:
		req.ProjectPath = d.com.Workspace.WorkingDir()
	}
	return func() tea.Msg {
		// The dialog has no context of its own to cancel — it is torn
		// down by being dropped — so this bounds the query itself. A
		// global sweep over a large history is the case worth bounding.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap, err := ws.Stats(ctx, req)
		return StatsLoadedMsg{Scope: scope, Snapshot: snap, Err: err}
	}
}

// HandleMsg implements [Dialog].
func (d *Stats) HandleMsg(msg tea.Msg) Action {
	switch msg := msg.(type) {
	case StatsLoadedMsg:
		for i, tab := range statsTabs {
			if tab.scope != msg.Scope {
				continue
			}
			d.loading[i] = false
			if msg.Err != nil {
				d.errs[i] = msg.Err
				break
			}
			d.snapshots[i] = msg.Snapshot
			break
		}
		return nil
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, d.keyMap.Close):
			return ActionClose{}
		case key.Matches(msg, d.keyMap.Next):
			d.active = (d.active + 1) % len(statsTabs)
			if cmd := d.loadTab(d.active); cmd != nil {
				return ActionCmd{Cmd: cmd}
			}
			return nil
		case key.Matches(msg, d.keyMap.Prev):
			d.active = (d.active - 1 + len(statsTabs)) % len(statsTabs)
			if cmd := d.loadTab(d.active); cmd != nil {
				return ActionCmd{Cmd: cmd}
			}
			return nil
		}
	}
	return nil
}

// ShortHelp implements [help.KeyMap].
func (d *Stats) ShortHelp() []key.Binding {
	return []key.Binding{d.keyMap.Next, d.keyMap.Prev, d.keyMap.Close}
}

// FullHelp implements [help.KeyMap].
func (d *Stats) FullHelp() [][]key.Binding { return [][]key.Binding{d.ShortHelp()} }

// Draw implements [Dialog].
func (d *Stats) Draw(scr uv.Screen, area uv.Rectangle) *tea.Cursor {
	t := d.com.Styles
	d.Resize(area)
	width := d.Width()
	innerWidth := max(0, d.InnerWidth())

	rc := NewRenderContext(t, width)
	rc.Title = "Usage"
	rc.AddPart(d.renderTabs(innerWidth))
	rc.AddPart(d.renderBody(innerWidth))
	rc.Help = renderDialogHelp(t, &d.help, d, innerWidth)

	DrawCenter(scr, area, rc.Render())
	return nil
}

// renderTabs draws the scope selector.
func (d *Stats) renderTabs(width int) string {
	t := d.com.Styles
	parts := make([]string, 0, len(statsTabs))
	for i, tab := range statsTabs {
		style := t.Threads.TabInactive
		if i == d.active {
			style = t.Threads.TabActive
		}
		parts = append(parts, style.Render(tab.label))
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(parts, " "))
}

// renderBody draws the active tab: its totals, then the breakdowns.
func (d *Stats) renderBody(width int) string {
	t := d.com.Styles
	i := d.active

	if err, ok := d.errs[i]; ok {
		return t.Status.ErrorMessage.Render("Could not read usage: " + err.Error())
	}
	if statsTabs[i].scope == stats.ScopeSession && d.sessionID == "" {
		return t.Resource.AdditionalText.Render("No active session yet — start one and its usage shows up here.")
	}
	snap, ok := d.snapshots[i]
	if !ok {
		return t.Resource.AdditionalText.Render("Reading usage…")
	}
	if snap.Empty() {
		return t.Resource.AdditionalText.Render("Nothing recorded in this scope yet.")
	}

	sections := []string{d.renderTotals(snap, width)}
	if models := d.renderModels(snap, width); models != "" {
		sections = append(sections, models)
	}
	if agents := d.renderAgents(snap, width); agents != "" {
		sections = append(sections, agents)
	}
	if projects := d.renderProjects(snap, width); projects != "" {
		sections = append(sections, projects)
	}
	if note := d.renderNote(snap, width); note != "" {
		sections = append(sections, note)
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// renderTotals draws the headline figures for the scope.
func (d *Stats) renderTotals(snap stats.Snapshot, width int) string {
	t := d.com.Styles
	label := t.Threads.DetailLabel
	value := t.Threads.DetailValue

	// One padded label column so the values line up: the block is read
	// down the value side, not the label side.
	row := func(name, v string) string {
		return label.Render(fmt.Sprintf("%-8s", name)) + " " + value.Render(v)
	}
	lines := []string{
		row("tokens", fmt.Sprintf("%s in / %s out",
			humanize.Comma(snap.Totals.PromptTokens), humanize.Comma(snap.Totals.CompletionTokens))),
		row("cost", fmt.Sprintf("$%.2f", snap.Totals.Cost)),
		row("time", formatStatsDuration(snap.Totals.TimeSeconds)),
		row("sessions", humanize.Comma(snap.Totals.Sessions)),
	}
	if snap.Outcome.Total > 0 {
		lines = append(lines, row("landed", fmt.Sprintf("%d of %d delegations (%.0f%%)",
			snap.Outcome.Landed, snap.Outcome.Total, snap.Outcome.Rate()*100)))
	}
	return lipgloss.NewStyle().Width(width).Render(lipgloss.JoinVertical(lipgloss.Left, lines...))
}

// renderModels draws the per-model breakdown.
func (d *Stats) renderModels(snap stats.Snapshot, width int) string {
	if len(snap.Models) == 0 {
		return ""
	}
	t := d.com.Styles
	var maxTokens int64
	for _, m := range snap.Models {
		maxTokens = max(maxTokens, m.Tokens())
	}

	// The same model id can be served by more than one provider, and each
	// is its own row with its own cost. Naming only the model would make
	// those read as a duplicated row, so the provider is appended exactly
	// where it disambiguates and nowhere else.
	idCounts := make(map[string]int, len(snap.Models))
	for _, m := range snap.Models {
		idCounts[m.Model]++
	}

	rows := make([]string, 0, statsRowsPerSection)
	for i, m := range snap.Models {
		if i >= statsRowsPerSection {
			rows = append(rows, t.Resource.AdditionalText.Render(
				fmt.Sprintf("…and %d more", len(snap.Models)-statsRowsPerSection)))
			break
		}
		name := m.Model
		if idCounts[m.Model] > 1 && m.Provider != "" {
			name += " (" + m.Provider + ")"
		}
		if m.Approximate {
			name = "~" + name
		}
		rows = append(rows, statsRow(t, name, m.Tokens(), maxTokens, width,
			formatStatsDuration(m.TimeSeconds), outcomeSuffix(m.Delegations, m.Succeeded)))
	}
	return section(t, "By model", width, rows)
}

// renderAgents draws the per-subagent breakdown.
func (d *Stats) renderAgents(snap stats.Snapshot, width int) string {
	if len(snap.Agents) == 0 {
		return ""
	}
	t := d.com.Styles
	var maxTokens int64
	for _, a := range snap.Agents {
		maxTokens = max(maxTokens, a.Tokens())
	}

	rows := make([]string, 0, statsRowsPerSection)
	for i, a := range snap.Agents {
		if i >= statsRowsPerSection {
			rows = append(rows, t.Resource.AdditionalText.Render(
				fmt.Sprintf("…and %d more", len(snap.Agents)-statsRowsPerSection)))
			break
		}
		rows = append(rows, statsRow(t, a.Name, a.Tokens(), maxTokens, width,
			formatStatsDuration(a.TimeSeconds), outcomeSuffix(a.Delegations, a.Succeeded)))
	}
	return section(t, "By subagent", width, rows)
}

// renderProjects draws the per-project breakdown, which only the global
// scope fills in. The trailing TOTAL row the aggregation appends is
// dropped here: the totals block above already says it.
func (d *Stats) renderProjects(snap stats.Snapshot, width int) string {
	if len(snap.Projects) == 0 {
		return ""
	}
	t := d.com.Styles
	projects := snap.Projects
	if last := projects[len(projects)-1]; last.Path == "TOTAL" {
		projects = projects[:len(projects)-1]
	}
	if len(projects) == 0 {
		return ""
	}

	var maxTokens int64
	for _, p := range projects {
		maxTokens = max(maxTokens, p.Tokens())
	}
	rows := make([]string, 0, statsRowsPerSection)
	for i, p := range projects {
		if i >= statsRowsPerSection {
			rows = append(rows, t.Resource.AdditionalText.Render(
				fmt.Sprintf("…and %d more", len(projects)-statsRowsPerSection)))
			break
		}
		name := p.Path
		if name == "" {
			name = "(unknown)"
		}
		rows = append(rows, statsRow(t, shortenPath(name), p.Tokens(), maxTokens, width,
			formatStatsDuration(p.TimeSeconds), ""))
	}
	return section(t, "By project", width, rows)
}

// renderNote spells out what the numbers above do and do not mean. It is
// deliberately not optional decoration: a proportional split and a
// "landed" count both read as exact unless they say otherwise.
func (d *Stats) renderNote(snap stats.Snapshot, width int) string {
	t := d.com.Styles
	var notes []string
	for _, m := range snap.Models {
		if m.Approximate {
			notes = append(notes, "~ tokens split across the models a session used, not measured per model")
			break
		}
	}
	if snap.Outcome.Total > 0 {
		notes = append(notes, "landed = the delegation finished or merged; review verdicts are not recorded")
	}
	if len(notes) == 0 {
		return ""
	}
	return lipgloss.NewStyle().Width(width).Render(
		"\n" + t.Resource.AdditionalText.Render(strings.Join(notes, "\n")))
}

// section renders a titled block of rows, with a blank line above it so
// the blocks read as separate rather than as one long column.
func section(t *styles.Styles, title string, width int, rows []string) string {
	head := common.Section(t, t.Resource.Heading.Render(title), width)
	return lipgloss.NewStyle().Width(width).Render(
		"\n" + head + "\n" + lipgloss.JoinVertical(lipgloss.Left, rows...))
}

// statsRow renders one breakdown row: name, share bar, token total, and
// whatever trailing figures the section carries.
func statsRow(t *styles.Styles, name string, tokens, maxTokens int64, width int, extras ...string) string {
	const nameWidth = 26
	label := t.Resource.Name.Render(truncate(name, nameWidth))
	pad := max(0, nameWidth-lipgloss.Width(truncate(name, nameWidth)))

	trailing := make([]string, 0, len(extras)+1)
	trailing = append(trailing, fmt.Sprintf("%10s", humanize.Comma(tokens)))
	for _, e := range extras {
		if e != "" {
			trailing = append(trailing, e)
		}
	}

	row := label + strings.Repeat(" ", pad) + " " +
		t.Threads.Subtle.Render(statsBar(tokens, maxTokens)) + " " +
		t.Threads.DetailValue.Render(strings.Join(trailing, "  "))
	return lipgloss.NewStyle().MaxWidth(width).Render(row)
}

// statsBar draws a proportional bar. It uses block characters rather than
// a numeric percentage because the question this column answers — which
// row dominates — is one the eye answers faster than arithmetic does.
func statsBar(value, maxValue int64) string {
	if maxValue <= 0 || value <= 0 {
		return strings.Repeat("·", statsBarWidth)
	}
	filled := int(float64(value) / float64(maxValue) * statsBarWidth)
	filled = max(0, min(statsBarWidth, filled))
	// A non-zero value always shows at least one block: rounding a real
	// row down to an empty bar reads as "nothing here", which is wrong.
	if filled == 0 {
		filled = 1
	}
	return strings.Repeat("█", filled) + strings.Repeat("·", statsBarWidth-filled)
}

// outcomeSuffix renders a row's delegation tally, or nothing when the row
// never ran one.
func outcomeSuffix(total, landed int64) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d ok", landed, total)
}

// truncate shortens s to width, marking the cut with an ellipsis.
func truncate(s string, width int) string {
	if width <= 1 || lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width-1]) + "…"
}

// shortenPath renders a project path by its last two segments, which is
// what distinguishes projects from each other in practice; the full path
// is rarely what the reader is scanning for.
func shortenPath(path string) string {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) <= 2 {
		return path
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// formatStatsDuration renders seconds compactly, rounding to the second.
func formatStatsDuration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

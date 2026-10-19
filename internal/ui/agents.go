package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/betta-tech/byo-coding-agent/internal/subagent"
)

// The agents sidebar is modeled on herdr: one row per agent with a
// colored state dot, so a glance tells you who's working, who's stuck
// waiting on you, and who's done. It appears the first time an agent is
// spawned and lives to the right of the conversation.

// Event is shorthand for the fleet's timeline entry.
type Event = subagent.Event

// FleetRefreshMsg tells the TUI a background agent changed (state, tool
// call, log line, diffstat). Sent from subagent.Background.OnChange.
type FleetRefreshMsg struct{}

const (
	sidebarWidth        = 38
	sidebarMinConvWidth = 50 // below this the sidebar would squeeze the chat; hide it
	sidebarRowLines     = 4  // three lines per agent plus a blank separator
)

var stateColors = map[subagent.State]lipgloss.Color{
	subagent.StateWorking:   "220", // yellow
	subagent.StateBlocked:   "203", // red: needs you
	subagent.StateDone:      "39",  // blue
	subagent.StateFailed:    "203",
	subagent.StateMerged:    "114", // green
	subagent.StateDiscarded: "240",
}

var stateIcons = map[subagent.State]string{
	subagent.StateBlocked:   "●",
	subagent.StateDone:      "●",
	subagent.StateFailed:    "✗",
	subagent.StateMerged:    "✓",
	subagent.StateDiscarded: "○",
}

func (m *harness) agentsVisible() bool {
	return len(subagent.Background.Snapshot()) > 0 && m.width-sidebarWidth >= sidebarMinConvWidth
}

// viewSidebar renders the panel at the given outer height.
func (m harness) viewSidebar(height int) string {
	agents := subagent.Background.Snapshot()
	inner := sidebarWidth - 4 // border + padding

	var rows []string
	counts := map[subagent.State]int{}
	for i, a := range agents {
		counts[a.State]++
		rows = append(rows, m.sidebarRow(a, inner, m.agentsFocus && i == m.agentsSelected))
	}

	// Footer: herdr-style tally, most urgent first.
	var tally []string
	for _, s := range []subagent.State{subagent.StateBlocked, subagent.StateWorking, subagent.StateDone, subagent.StateFailed} {
		if counts[s] > 0 {
			dot := lipgloss.NewStyle().Foreground(stateColors[s]).Render("●")
			tally = append(tally, fmt.Sprintf("%s %d %s", dot, counts[s], s))
		}
	}
	footer := lipgloss.NewStyle().MaxWidth(inner).Render(strings.Join(tally, "  "))
	if m.agentsFocus {
		footer = Dimmed("↑↓ enter:view m:merge x:discard")
	}

	body := strings.Join(rows, "\n\n")
	bodyH := max(height-2-2, 1) // border rows, footer + gap
	lines := strings.Split(body, "\n")
	if len(lines) > bodyH {
		start := m.sidebarScroll(len(lines), bodyH)
		lines = lines[start : start+bodyH]
	}
	for len(lines) < bodyH {
		lines = append(lines, "")
	}

	title := lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Bold(true).Render("agents")
	border := lipgloss.Color("240")
	if m.agentsFocus {
		border = lipgloss.Color("36")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(0, 1).
		Width(sidebarWidth - 2).
		Height(height - 2).
		Render(title + "\n" + strings.Join(lines, "\n") + "\n" + footer)
}

// sidebarScroll is the first visible body line when the list is taller
// than the panel: enough to keep the selected agent on screen.
func (m harness) sidebarScroll(total, bodyH int) int {
	if total <= bodyH || !m.agentsFocus {
		return 0
	}
	return max(min(m.agentsSelected*sidebarRowLines, total-bodyH), 0)
}

// sidebarRow is three lines: name + elapsed, state + diffstat, activity.
func (m harness) sidebarRow(a subagent.Status, width int, selected bool) string {
	color := stateColors[a.State]
	icon := stateIcons[a.State]
	if a.State == subagent.StateWorking {
		icon = strings.TrimSpace(lipgloss.NewStyle().Foreground(color).Render(m.spinner.Spinner.Frames[m.spinnerFrame()]))
	} else {
		icon = lipgloss.NewStyle().Foreground(color).Render(icon)
	}

	elapsed := Dimmed(formatElapsed(a.Elapsed()))
	name := lipgloss.NewStyle().Bold(true).Render(clip(a.Name, width-10))
	gap := max(width-lipgloss.Width(icon+" "+name)-lipgloss.Width(elapsed), 1)
	line1 := icon + " " + name + strings.Repeat(" ", gap) + elapsed

	label := a.State.String()
	if a.State == subagent.StateBlocked {
		label = "needs approval"
	}
	line2 := "  " + lipgloss.NewStyle().Foreground(color).Render(label) + Dimmed(" · ") + diffLabel(a.Added, a.Removed) +
		Dimmed(fmt.Sprintf(" · %d tool%s", a.ToolCalls, plural(a.ToolCalls)))

	line3 := "  " + Dimmed(clip(a.Activity, width-2))

	// A wrapped line would push the panel taller than the conversation
	// it sits beside, so every line is clipped to the panel instead.
	fit := lipgloss.NewStyle().MaxWidth(width)
	row := fit.Render(line1) + "\n" + fit.Render(line2) + "\n" + fit.Render(line3)
	if selected {
		row = lipgloss.NewStyle().Background(lipgloss.Color("237")).Width(width).Render(row)
	}
	return row
}

// spinnerFrame derives a frame index from the clock so every working
// agent animates in lockstep without each one owning a spinner.Model.
func (m harness) spinnerFrame() int {
	sp := m.spinner.Spinner
	return int(time.Now().UnixMilli()/sp.FPS.Milliseconds()) % len(sp.Frames)
}

// updateAgentsFocus handles keys while the sidebar has focus. With the
// agent view open, ↑/↓ still switch agents so you can flip between them
// and watch each one work; scrolling the view is pgup/pgdn.
func (m harness) updateAgentsFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	agents := subagent.Background.Snapshot()
	if len(agents) == 0 {
		m.agentsFocus, m.agentDetail = false, false
		m.input.Focus()
		m.layout()
		return m, nil
	}
	m.agentsSelected = min(m.agentsSelected, len(agents)-1)
	selected := agents[m.agentsSelected].Name

	switch msg.String() {
	case "ctrl+c", "ctrl+d":
		return m, tea.Quit
	case "esc":
		if m.agentDetail {
			m.agentDetail = false
		} else {
			m.agentsFocus = false
			m.input.Focus()
		}
		m.layout()
		return m, nil
	case "enter":
		m.openAgent(m.agentsSelected)
		return m, nil
	case "up", "k":
		m.selectAgent(max(m.agentsSelected-1, 0))
		return m, nil
	case "down", "j":
		m.selectAgent(min(m.agentsSelected+1, len(agents)-1))
		return m, nil
	case "m":
		return m, fleetCmd(func() { MergeAgent(selected) })
	case "x":
		return m, fleetCmd(func() { DiscardAgent(selected) })
	}
	if m.agentDetail {
		var cmd tea.Cmd
		m.agentView, cmd = m.agentView.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *harness) openAgent(i int) {
	m.agentsFocus, m.agentDetail = true, true
	m.input.Blur()
	m.agentsSelected = i
	m.layout()
	m.refreshAgentView(true)
}

func (m *harness) selectAgent(i int) {
	changed := i != m.agentsSelected
	m.agentsSelected = i
	if m.agentDetail && changed {
		m.refreshAgentView(true)
	}
}

// mouseAgents handles clicks on the sidebar (select + open that agent) and
// wheel scrolling inside the agent view. Reports whether it consumed msg.
func (m harness) mouseAgents(msg tea.MouseMsg) (harness, bool) {
	if !m.agentsVisible() {
		return m, false
	}
	inPane := msg.X < m.viewport.Width && msg.Y < m.viewport.Height
	if m.agentDetail && inPane && msg.Action == tea.MouseActionPress &&
		(msg.Button == tea.MouseButtonWheelUp || msg.Button == tea.MouseButtonWheelDown) {
		m.agentView, _ = m.agentView.Update(msg)
		return m, true
	}
	if msg.X < m.viewport.Width || msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, false
	}
	// Sidebar rows start under the top border and the title.
	agents := subagent.Background.Snapshot()
	bodyH := max(m.viewport.Height-4, 1)
	line := msg.Y - 2
	if line < 0 || line >= bodyH {
		return m, false
	}
	line += m.sidebarScroll(len(agents)*sidebarRowLines-1, bodyH)
	if i := line / sidebarRowLines; i < len(agents) {
		m.openAgent(i)
		return m, true
	}
	return m, false
}

// refreshAgentView re-renders the selected agent's timeline. It follows
// the tail unless the user has scrolled up to read something.
func (m *harness) refreshAgentView(forceBottom bool) {
	agents := subagent.Background.Snapshot()
	if m.agentsSelected >= len(agents) {
		return
	}
	follow := forceBottom || m.agentView.AtBottom()
	events := subagent.Background.Events(agents[m.agentsSelected].Name)
	m.agentView.SetContent(renderTimeline(events, m.agentView.Width))
	if follow {
		m.agentView.GotoBottom()
	}
}

// viewAgentPane takes the conversation's place while an agent is open:
// a header with the agent's vitals, then its timeline.
func (m harness) viewAgentPane() string {
	agents := subagent.Background.Snapshot()
	if m.agentsSelected >= len(agents) {
		return m.viewport.View()
	}
	a := agents[m.agentsSelected]
	w := m.viewport.Width
	color := stateColors[a.State]

	icon := lipgloss.NewStyle().Foreground(color).Render(stateIcons[a.State])
	if a.State == subagent.StateWorking {
		icon = lipgloss.NewStyle().Foreground(color).Render(m.spinner.Spinner.Frames[m.spinnerFrame()])
	}
	left := icon + " " + lipgloss.NewStyle().Bold(true).Render(a.Name) + Dimmed(" · ") +
		lipgloss.NewStyle().Foreground(color).Render(a.State.String()) + "   " + diffLabel(a.Added, a.Removed) +
		Dimmed(fmt.Sprintf(" · %d tool%s", a.ToolCalls, plural(a.ToolCalls)))
	right := Dimmed(a.Branch + "  " + formatElapsed(a.Elapsed()))
	gap := max(w-2-lipgloss.Width(left)-lipgloss.Width(right), 1)
	header := left + strings.Repeat(" ", gap) + right
	task := Dimmed(clip("task  "+strings.Join(strings.Fields(a.Task), " "), w-2))
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(strings.Repeat("─", max(w-2, 1)))

	fit := lipgloss.NewStyle().MaxWidth(w - 2)
	return lipgloss.NewStyle().Padding(0, 1).Width(w).Height(m.viewport.Height).Render(
		fit.Render(header) + "\n" + task + "\n" + sep + "\n" + m.agentView.View())
}

var toolColors = map[string]lipgloss.Color{
	"bash":       "214",
	"read_file":  "39",
	"write_file": "114",
}

// renderTimeline formats an agent's events for the agent view:
//
//	14:02:11  ▸ write_file cache.go                     ✓ 3ms
//	14:02:12  ▸ bash go test ./...                      ✗ 1.2s
//	            --- FAIL: TestEvict (0.00s)
//	14:02:15  The LRU evicts on Put; tests pass now.
func renderTimeline(events []Event, width int) string {
	if len(events) == 0 {
		return Dimmed("waiting for the first step…")
	}
	const indent = 10 // "15:04:05" + 2 spaces
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	for i, e := range events {
		ts := Dimmed(e.Time.Format("15:04:05")) + "  "
		if e.Tool == "" {
			if i > 0 {
				b.WriteString("\n")
			}
			body := lipgloss.NewStyle().Width(max(width-indent, 20)).
				Foreground(lipgloss.Color("253")).Render(e.Text)
			for j, l := range strings.Split(body, "\n") {
				if j == 0 {
					b.WriteString(ts + l + "\n")
				} else {
					b.WriteString(pad + l + "\n")
				}
			}
			b.WriteString("\n")
			continue
		}

		status := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render("…")
		if e.Done && e.IsErr {
			status = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render("✗ " + formatDuration(e.Elapsed))
		} else if e.Done {
			status = lipgloss.NewStyle().Foreground(lipgloss.Color("114")).Render("✓ " + formatDuration(e.Elapsed))
		}
		color, ok := toolColors[e.Tool]
		if !ok {
			color = "141"
		}
		name := lipgloss.NewStyle().Foreground(color).Bold(true).Render(e.Tool)
		arg := clip(strings.Join(strings.Fields(e.Text), " "), max(width-indent-lipgloss.Width(e.Tool)-4-lipgloss.Width(status)-2, 10))
		call := Dimmed("▸ ") + name + " " + arg
		gap := max(width-indent-lipgloss.Width(call)-lipgloss.Width(status), 1)
		b.WriteString(ts + call + strings.Repeat(" ", gap) + status + "\n")

		for _, l := range resultPreview(e) {
			style := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
			if e.IsErr {
				style = style.Foreground(lipgloss.Color("174"))
			}
			b.WriteString(pad + "  " + style.Render(clip(l, max(width-indent-2, 10))) + "\n")
		}
	}
	return b.String()
}

// resultPreview picks the few lines of a tool result worth showing. File
// contents are summarized (you asked what the agent is doing, not for the
// file); command output shows its tail, where test summaries and errors land.
func resultPreview(e Event) []string {
	if !e.Done || strings.TrimSpace(e.Result) == "" {
		return nil
	}
	all := strings.Split(strings.TrimRight(e.Result, "\n"), "\n")
	if e.Tool == "read_file" && !e.IsErr {
		return []string{fmt.Sprintf("%d lines", len(all))}
	}
	var lines []string
	for _, l := range all {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	const keep = 4
	if len(lines) <= keep {
		return lines
	}
	if e.Tool == "bash" || e.IsErr {
		return append([]string{fmt.Sprintf("… %d earlier lines", len(lines)-keep)}, lines[len(lines)-keep:]...)
	}
	return append(lines[:keep], fmt.Sprintf("… %d more lines", len(lines)-keep))
}

func diffLabel(added, removed int) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("114")).Render(fmt.Sprintf("+%d", added)) + " " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render(fmt.Sprintf("−%d", removed))
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// Merge and discard run git, so they happen off the UI goroutine. Output
// goes to stdout, which main pipes into the scrollback.
func fleetCmd(fn func()) tea.Cmd {
	return func() tea.Msg {
		fn()
		return FleetRefreshMsg{}
	}
}

// MergeAgent and DiscardAgent are shared by the sidebar keys and the
// /merge and /discard commands.
func MergeAgent(name string) {
	out, err := subagent.Background.Merge(name)
	if err != nil {
		fmt.Println(Dimmed("merge " + name + ": " + err.Error()))
		return
	}
	if out != "" {
		fmt.Println(out)
	}
	fmt.Println(Dimmed("✓ merged " + name + " and removed its worktree"))
}

func DiscardAgent(name string) {
	if err := subagent.Background.Discard(name); err != nil {
		fmt.Println(Dimmed("discard " + name + ": " + err.Error()))
		return
	}
	fmt.Println(Dimmed("○ discarded " + name))
}

func formatElapsed(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// clip shortens plain text to n display columns.
func clip(s string, n int) string {
	if n <= 1 || lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > n-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

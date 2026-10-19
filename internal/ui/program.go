package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/betta-tech/byo-coding-agent/internal/api"
	"github.com/betta-tech/byo-coding-agent/internal/debug"
	"github.com/betta-tech/byo-coding-agent/internal/mcp"
	"github.com/betta-tech/byo-coding-agent/internal/subagent"
)

// UsageFunc returns the session's cumulative token usage and estimated cost
// in USD. Cost is -1 if unknown (e.g. unrecognized model). The TUI renders
// this on the status line when idle. Optional — pass nil to disable.
type UsageFunc func() (api.Usage, float64)

// debugPanelHeight is the total vertical space the debug panel occupies when
// shown, including its border. Conversation viewport shrinks by this much.
const debugPanelHeight = 12

// debugModalMaxWidth / debugModalMaxHeight cap how big the detail modal can
// grow; smaller terminals get a near-full-screen modal with a 2-row margin.
const (
	debugModalMaxWidth  = 140
	debugModalMaxHeight = 50
	debugModalMinHeight = 10
)

// AgentRunner is what the program calls when the user submits a line. It
// covers both slash commands and normal agent turns — the harness wires it
// up in main, so the program doesn't need to know about commands or agents.
type AgentRunner func(ctx context.Context, input string) error

// ── messages ──────────────────────────────────────────────────────────────

// AppendMsg appends text to the scrollback. Sent by the stdout-pipe forwarder.
type AppendMsg string

// ApprovalRequest is sent (via program.Send) when the agent needs y/n from
// the user. The program shows the prompt and writes the answer to Reply.
//
// Detail is optional long-form content. When non-empty, the TUI renders a
// full-screen modal with the detail (syntax-highlighted if it looks like
// a diff or JSON) instead of the inline single-line y/n box — used by
// write_file so the user can review the unified diff before approving.
type ApprovalRequest struct {
	Prompt string
	Detail string
	Reply  chan bool
}

// DebugRefreshMsg tells the TUI to re-read the debug ring and redraw the
// panel. Sent from debug.SetSink whenever a new event is recorded.
type DebugRefreshMsg struct{}

// DebugToggleMsg tells the TUI that debug.Enabled() may have changed, so it
// should recompute viewport heights. Sent by the /debug slash command.
type DebugToggleMsg struct{}

// MCPStatusMsg reports progress while MCP servers are being connected in
// the background at startup. The TUI shows a "loading…" line above the
// input box until ProgressDone arrives.
type MCPStatusMsg struct {
	Server string
	Status mcp.ProgressStatus
	Total  int
}

// shineTickMsg drives the startup banner shine animation. Self-perpetuates
// until the shine peak passes off the right edge of the banner.
type shineTickMsg struct{}

// internal messages
type agentDoneMsg struct{ err error }

// ── model ─────────────────────────────────────────────────────────────────

type modelState int

const (
	stateIdle modelState = iota
	stateRunning
	stateAwaitingApproval
)

type harness struct {
	runner    AgentRunner
	usageFunc UsageFunc

	width, height int

	viewport  viewport.Model
	debugView viewport.Model // bottom panel, visible only when debug.Enabled()
	input     textinput.Model
	spinner   spinner.Model

	state            modelState
	preApprovalState modelState // restored after y/n: background agents can ask while the root is idle
	approvalPrompt   string
	approvalDetail   string // optional diff/payload shown in the approval modal
	approvalReply    chan bool
	followBottom     bool // auto-scroll viewport to bottom on new content

	// debug-panel interaction state
	debugFocus    bool      // Tab moves focus between input and debug panel
	debugMode     debugMode // list (overview) or detail (one event's payload)
	debugSelected uint64    // ID of the currently-selected event

	// agents-sidebar interaction state (see agents.go)
	agentsFocus    bool
	agentsSelected int
	agentDetail    bool           // log modal for the selected agent is open
	agentView      viewport.Model // hosts that log

	// banner shine animation state. bannerEnd is the byte offset in output
	// where the banner ends, so each tick can swap the banner without
	// touching the rest of the scrollback. bannerWidth is captured at
	// construction — it has to be, because by the time Init() runs main.go
	// has already redirected os.Stdout into the alt-screen pipe and
	// TermWidth() comes back as zero.
	shineCol    int
	shineTick   int  // frame counter; eased into shineCol each frame
	shineDone   bool
	bannerEnd   int
	bannerWidth int

	// MCP background-loading state. mcpLoading is true between the
	// ProgressBegin and ProgressDone messages; the status line shows the
	// current server and an X/N counter while connections are in flight.
	mcpLoading bool
	mcpTotal   int
	mcpDone    int
	mcpFailed  int
	mcpCurrent string

	// output must be a pointer: Bubble Tea passes the model by value through
	// Update, and strings.Builder panics when copied (copyCheck on its
	// internal self-pointer). The pointer survives the copy intact.
	output *strings.Builder
}

type debugMode int

const (
	debugList   debugMode = iota // overview list, one line per event
	debugDetail                  // one event's payload, scrollable
)

func newHarness(runner AgentRunner, usageFunc UsageFunc, banner string) harness {
	ti := textinput.New()
	ti.Prompt = "❯ "
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Bold(true)
	ti.CharLimit = 0
	ti.Focus()

	vp := viewport.New(80, 20)
	vp.SetContent(banner)

	dv := viewport.New(80, debugPanelHeight-2)
	dv.SetContent(Dimmed("(no debug events yet)"))

	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Bold(true)

	m := harness{
		runner:       runner,
		input:        ti,
		viewport:     vp,
		debugView:    dv,
		agentView:    viewport.New(80, 20),
		spinner:      sp,
		followBottom: true,
		// shineCol starts off-screen left so the very first frame shows
		// the banner with no highlight visible — the streak slides in
		// as ticks fire and the easing function ramps it across.
		shineCol: -shineEdgePad,
		output:   &strings.Builder{},
	}
	m.output.WriteString(banner)
	m.bannerEnd = m.output.Len()
	return m
}

// NewProgram builds the Bubble Tea program. Wire the returned program to
// a Confirm callback and a stdout-pipe forwarder before calling Run. Pass
// nil for usageFunc to omit the token-usage line on the status bar.
//
// We capture the terminal width here, before main.go redirects os.Stdout
// to the alt-screen pipe — once the redirect happens TermWidth() reports
// zero, which is too late for the banner animation to detect "this
// terminal can render the wide wordmark."
func NewProgram(runner AgentRunner, usageFunc UsageFunc) *tea.Program {
	width := TermWidth()
	banner := BannerText(width)
	m := newHarness(runner, usageFunc, banner)
	m.usageFunc = usageFunc
	m.bannerWidth = width
	return tea.NewProgram(
		m,
		tea.WithAltScreen(),
		tea.WithMouseAllMotion(),
	)
}

func (m harness) Init() tea.Cmd {
	// Animate the banner only when the wide variant is in play; the narrow
	// fallback is a single line and looks weird flickering. Width was
	// captured in NewProgram before main redirected stdout to the pipe.
	if m.bannerWidth >= bigBannerMinWidth {
		return tea.Batch(textinput.Blink, shineTickCmd())
	}
	return textinput.Blink
}

// shineTotalTicks / shineFrameMs together set how long the startup shine
// sweeps for: ~2.4 s with an ease-in-out curve. shineEdgePad is how far
// the peak overshoots each side of the wordmark before stopping, so the
// streak enters and exits fully off-screen.
const (
	shineTotalTicks = 60
	shineFrameMs    = 40
	shineEdgePad    = 15
)

// shineTickCmd schedules the next shine animation frame.
func shineTickCmd() tea.Cmd {
	return tea.Tick(shineFrameMs*time.Millisecond, func(time.Time) tea.Msg {
		return shineTickMsg{}
	})
}

// easeInOutCubic maps t ∈ [0,1] to [0,1] with a smooth-start/smooth-end
// shape: slow entrance, fast middle, slow exit. The classic curve for
// "object passing through" motion.
func easeInOutCubic(t float64) float64 {
	if t < 0 {
		return 0
	}
	if t > 1 {
		return 1
	}
	if t < 0.5 {
		return 4 * t * t * t
	}
	p := 2*t - 2
	return 0.5*p*p*p + 1
}

func (m harness) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		// Re-wrap the conversation scrollback to the new viewport width.
		// Without this, lines wrapped at the old width stay that way after
		// a resize — fine when the terminal grew, ugly when it shrank.
		m.setConversationContent()
		// Re-wrap the debug panel/modal to the new size. Cheap and matters
		// most for detail mode, where lines were wrapped at the old width.
		if debug.Enabled() {
			m.refreshDebugContent()
		}
		if m.followBottom {
			m.viewport.GotoBottom()
		}
		return m, nil

	case DebugToggleMsg:
		m.layout()
		if !debug.Enabled() {
			// Panel was just turned off — return focus to input and reset
			// detail mode so it's not lingering when we re-enable later.
			m.debugFocus = false
			m.debugMode = debugList
			m.input.Focus()
		}
		m.refreshDebugContent()
		if m.followBottom {
			m.viewport.GotoBottom()
		}
		return m, nil

	case DebugRefreshMsg:
		if debug.Enabled() {
			m.refreshDebugContent()
		}
		return m, nil

	case FleetRefreshMsg:
		// The first spawn makes the sidebar appear, which narrows the
		// conversation — re-wrap it when that happens.
		prevW := m.viewport.Width
		m.layout()
		if m.viewport.Width != prevW {
			m.setConversationContent()
			if m.followBottom {
				m.viewport.GotoBottom()
			}
		}
		if m.agentDetail {
			m.refreshAgentView(false)
		}
		if subagent.Background.Busy() {
			return m, m.spinner.Tick // stale ticks are dropped by the spinner, so extra kicks are free
		}
		return m, nil

	case MCPStatusMsg:
		switch msg.Status {
		case mcp.ProgressBegin:
			if msg.Total == 0 {
				return m, nil
			}
			m.mcpLoading = true
			m.mcpTotal = msg.Total
			m.mcpDone = 0
			m.mcpFailed = 0
			m.mcpCurrent = ""
			// Kick the spinner so the loading line animates.
			return m, m.spinner.Tick
		case mcp.ProgressConnecting:
			m.mcpCurrent = msg.Server
		case mcp.ProgressConnected:
			m.mcpDone++
			m.mcpCurrent = ""
		case mcp.ProgressFailed:
			m.mcpDone++
			m.mcpFailed++
			m.mcpCurrent = ""
		case mcp.ProgressDone:
			m.mcpLoading = false
			m.mcpCurrent = ""
		}
		return m, nil

	case tea.MouseMsg:
		if m.state != stateAwaitingApproval {
			if mm, ok := m.mouseAgents(msg); ok {
				return mm, nil
			}
		}
		// Mouse only matters when the bottom panel is on screen. The modal
		// hides it; in detail mode we ignore clicks entirely.
		if !debug.Enabled() || m.debugMode == debugDetail {
			break
		}
		if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
			break
		}
		return m.mouseSelect(msg), nil

	case tea.KeyMsg:
		if m.state == stateAwaitingApproval {
			return m.updateApproval(msg)
		}
		// Tab cycles focus: input → agents sidebar → debug panel → input,
		// skipping whichever panels aren't on screen.
		if msg.Type == tea.KeyTab && (debug.Enabled() || m.agentsVisible()) {
			return m.cycleFocus(), nil
		}
		if m.agentsFocus {
			return m.updateAgentsFocus(msg)
		}
		if m.debugFocus {
			return m.updateDebugFocus(msg)
		}
		return m.updateInput(msg)

	case AppendMsg:
		m.output.WriteString(string(msg))
		m.setConversationContent()
		if m.followBottom {
			m.viewport.GotoBottom()
		}
		return m, nil

	case agentDoneMsg:
		if m.state == stateAwaitingApproval {
			// A background agent's question is on screen; go idle once
			// the user answers it instead of hiding it now.
			m.preApprovalState = stateIdle
		} else {
			m.state = stateIdle
		}
		if msg.err != nil {
			m.output.WriteString(Dimmed(fmt.Sprintf("error: %v", msg.err)) + "\n")
			m.setConversationContent()
			if m.followBottom {
				m.viewport.GotoBottom()
			}
		}
		return m, nil

	case ApprovalRequest:
		m.preApprovalState = m.state
		m.state = stateAwaitingApproval
		m.approvalPrompt = msg.Prompt
		m.approvalDetail = msg.Detail
		m.approvalReply = msg.Reply
		if m.approvalDetail != "" {
			// Reuse debugView (it's just a viewport) to host the diff
			// inside the approval modal. Set its size to the modal body
			// and fill it with the syntax-highlighted detail.
			m.layout()
			m.debugView.SetContent(HighlightPayload(m.approvalDetail, m.debugView.Width))
			m.debugView.GotoTop()
		}
		return m, nil

	case shineTickMsg:
		if m.shineDone {
			return m, nil
		}
		m.shineTick++
		if m.shineTick > shineTotalTicks {
			// Sweep complete: render once more in static mode and stop.
			m.shineCol = -1
			m.shineDone = true
			m.replaceBanner()
			m.setConversationContent()
			return m, nil
		}
		// Map tick → shineCol through easeInOutCubic so the streak
		// glides in, accelerates across the wordmark, and decelerates
		// as it exits. Range spans the full width plus a margin on
		// each side so the shine is fully off-screen at the endpoints.
		progress := float64(m.shineTick) / float64(shineTotalTicks)
		eased := easeInOutCubic(progress)
		m.shineCol = int(eased*float64(BigBannerWidth+shineEdgePad*2)) - shineEdgePad
		m.replaceBanner()
		m.setConversationContent()
		if m.followBottom {
			m.viewport.GotoBottom()
		}
		return m, shineTickCmd()

	case spinner.TickMsg:
		// Keep ticking while either the agent is mid-call or MCP is still
		// connecting in the background. Otherwise the spinner pauses on
		// its current frame.
		if m.state != stateRunning && !m.mcpLoading && !subagent.Background.Busy() {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	// Forward unhandled events to the viewport (handles scroll keys).
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	m.followBottom = m.viewport.AtBottom()
	return m, cmd
}

func (m harness) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		if m.state != stateIdle {
			return m, nil
		}
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		// Echo the prompt into scrollback so the user can scan back through
		// the conversation. A timestamped divider above each turn gives an
		// obvious "this is a new prompt" marker — without it, tool log
		// lines and assistant text all run together visually.
		m.output.WriteString("\n" + renderTurnHeader(m.viewport.Width) + "\n")
		m.output.WriteString(promptMarkerStyle.Render("❯ ") + promptTextStyle.Render(text) + "\n\n")
		m.setConversationContent()
		m.viewport.GotoBottom()
		m.followBottom = true
		m.input.SetValue("")
		m.state = stateRunning
		// Kick the spinner alongside the agent run so it animates while
		// we wait. The TickMsg handler self-perpetuates while running.
		return m, tea.Batch(m.runOnce(text), m.spinner.Tick)

	case tea.KeyCtrlD, tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		m.followBottom = m.viewport.AtBottom()
		return m, cmd
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m harness) updateApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// When the approval includes a diff/detail, forward scroll keys to the
	// modal viewport so the user can browse long diffs. y/n still resolves.
	if m.approvalDetail != "" {
		switch msg.Type {
		case tea.KeyPgUp, tea.KeyPgDown, tea.KeyHome, tea.KeyEnd, tea.KeyUp, tea.KeyDown:
			var cmd tea.Cmd
			m.debugView, cmd = m.debugView.Update(msg)
			return m, cmd
		}
	}

	var answer bool
	switch msg.String() {
	case "y", "Y":
		answer = true
	case "n", "N", "esc", "ctrl+c", "ctrl+d", "enter":
		answer = false
	default:
		return m, nil
	}
	// Echo the decision into scrollback so the user can see what they answered.
	verdict := "denied"
	if answer {
		verdict = "approved"
	}
	m.output.WriteString(Dimmed(fmt.Sprintf("  ↳ %s", verdict)) + "\n")
	m.setConversationContent()
	m.viewport.GotoBottom()

	if m.approvalReply != nil {
		m.approvalReply <- answer
		m.approvalReply = nil
	}
	m.approvalPrompt = ""
	m.approvalDetail = ""
	m.state = m.preApprovalState
	// If the modal was up, layout needs to revert from modal-size to the
	// normal panel/viewport split before the next render.
	m.layout()
	return m, nil
}

func (m harness) runOnce(text string) tea.Cmd {
	return func() tea.Msg {
		err := m.runner(context.Background(), text)
		return agentDoneMsg{err: err}
	}
}

// layout recomputes viewport widths/heights from the current window size.
// In any modal state (debug detail OR approval-with-diff) the debug
// viewport is resized to fit the modal body; in every other state it
// goes back to the bottom-panel dimensions.
func (m *harness) layout() {
	m.input.Width = max(m.width-6, 20)

	if m.inModal() {
		modalW, modalH := m.modalSize()
		// Inside: outer border (2 cols/rows) + horizontal padding (2 cols)
		// + 2 rows for title + separator.
		m.debugView.Width = max(modalW-4, 20)
		m.debugView.Height = max(modalH-4, 4)
		return
	}

	inputHeight := 5 // spinner line + box (3) + hint (1) — always reserved
	debugH := 0
	if debug.Enabled() {
		debugH = debugPanelHeight
	}
	m.viewport.Width = m.width
	if m.agentsVisible() {
		m.viewport.Width = m.width - sidebarWidth
	}
	m.viewport.Height = max(m.height-inputHeight-debugH, 3)
	m.debugView.Width = max(m.width-2, 20)
	m.debugView.Height = max(debugPanelHeight-2, 1)
	// The agent view fills the conversation's slot: minus 1-col padding
	// each side, minus header, task and separator rows.
	m.agentView.Width = max(m.viewport.Width-2, 20)
	m.agentView.Height = max(m.viewport.Height-3, 1)
}

// inModal reports whether the current state should render a centered
// full-screen modal in place of the normal viewport+input layout. Two
// callers: the debug detail view (Enter on a panel entry) and the
// approval-with-diff flow (write_file proposal).
func (m *harness) inModal() bool {
	if m.debugMode == debugDetail && debug.Enabled() {
		return true
	}
	if m.state == stateAwaitingApproval && m.approvalDetail != "" {
		return true
	}
	return false
}

// modalSize returns the outer width/height of the detail modal — capped so
// it doesn't get unreadably wide on big monitors, and clamped so small
// terminals still get something usable.
func (m *harness) modalSize() (int, int) {
	w := m.width - 4
	if w > debugModalMaxWidth {
		w = debugModalMaxWidth
	}
	if w < 30 {
		w = 30
	}
	h := m.height - 4
	if h > debugModalMaxHeight {
		h = debugModalMaxHeight
	}
	if h < debugModalMinHeight {
		h = debugModalMinHeight
	}
	return w, h
}

// refreshDebugContent re-renders the debug ring into the debug viewport.
// Two modes:
//   - debugList: one line per event; the currently selected event (if any)
//     gets a background highlight. Auto-scrolls to the bottom when not
//     focused, follows the selection when focused.
//   - debugDetail: the selected event's full payload, scrollable.
//
// Cheap to call — Snapshot copies ~500 events at worst.
func (m *harness) refreshDebugContent() {
	if m.debugMode == debugDetail {
		m.renderDebugDetail()
		return
	}
	m.renderDebugList()
}

func (m *harness) renderDebugList() {
	events := debug.Snapshot()
	if len(events) == 0 {
		m.debugView.SetContent(Dimmed("(no debug events yet)"))
		return
	}
	selIdx := -1
	var sb strings.Builder
	for i, e := range events {
		line := formatDebugEvent(e)
		if m.debugFocus && e.ID == m.debugSelected {
			line = selectedLineStyle.Render(line)
			selIdx = i
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	m.debugView.SetContent(sb.String())
	if m.debugFocus && selIdx >= 0 {
		// Keep the selection roughly centered in the viewport so up/down
		// navigation feels stable.
		offset := selIdx - m.debugView.Height/2
		if offset < 0 {
			offset = 0
		}
		m.debugView.SetYOffset(offset)
	} else {
		m.debugView.GotoBottom()
	}
}

func (m *harness) renderDebugDetail() {
	e, ok := debug.FindByID(m.debugSelected)
	if !ok {
		m.debugView.SetContent(Dimmed(fmt.Sprintf("(event #%d is not in the ring anymore)", m.debugSelected)))
		return
	}
	body := e.Payload
	if body == "" {
		body = Dimmed("(no payload for this event)")
		m.debugView.SetContent(body)
		m.debugView.GotoTop()
		return
	}
	// Syntax-highlight JSON payloads and wrap long lines to the viewport
	// width so values, URLs, and no-break tool output snap into the modal
	// instead of spilling off the right edge. The modal's title bar is
	// rendered by viewDebugModal itself — viewport content is only the
	// payload.
	m.debugView.SetContent(HighlightPayload(body, m.debugView.Width))
	m.debugView.GotoTop()
}

// cycleFocus moves focus to the next visible pane.
func (m harness) cycleFocus() tea.Model {
	switch {
	case m.agentsFocus:
		m.agentsFocus, m.agentDetail = false, false
		m.layout()
		if debug.Enabled() {
			return m.toggleDebugFocus()
		}
		m.input.Focus()
	case m.debugFocus:
		return m.toggleDebugFocus()
	case m.agentsVisible():
		m.agentsFocus = true
		m.input.Blur()
	default:
		return m.toggleDebugFocus()
	}
	return m
}

// toggleDebugFocus flips focus between the input box and the debug panel.
// On the first focus, picks the latest event as the selection so ↑/↓ has a
// starting point.
func (m harness) toggleDebugFocus() tea.Model {
	m.debugFocus = !m.debugFocus
	if m.debugFocus {
		m.input.Blur()
		if m.debugSelected == 0 {
			events := debug.Snapshot()
			if len(events) > 0 {
				m.debugSelected = events[len(events)-1].ID
			}
		}
	} else {
		m.input.Focus()
	}
	// Always back to list mode on a focus change — Tab is a "reset" so the
	// user never gets stuck inside the modal after pressing it.
	m.debugMode = debugList
	m.layout()
	m.refreshDebugContent()
	return m
}

// updateDebugFocus is the key handler used while the debug panel is focused.
// It owns ↑/↓ navigation, Enter to drill in, Esc to back out, and forwards
// scroll keys to the viewport so PgUp/PgDn still work inside long payloads.
func (m harness) updateDebugFocus(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlD, tea.KeyCtrlC:
		return m, tea.Quit

	case tea.KeyEsc:
		if m.debugMode == debugDetail {
			m.debugMode = debugList
			m.layout()
			m.refreshDebugContent()
			return m, nil
		}
		m.debugFocus = false
		m.input.Focus()
		m.refreshDebugContent()
		return m, nil

	case tea.KeyEnter:
		if m.debugMode == debugList && m.debugSelected != 0 {
			m.debugMode = debugDetail
			m.layout()
			m.refreshDebugContent()
		}
		return m, nil

	case tea.KeyUp:
		if m.debugMode == debugList {
			m.moveSelection(-1)
			m.refreshDebugContent()
			return m, nil
		}

	case tea.KeyDown:
		if m.debugMode == debugList {
			m.moveSelection(+1)
			m.refreshDebugContent()
			return m, nil
		}

	case tea.KeyLeft, tea.KeyRight:
		// In detail mode, ←/→ jumps to the paired event (request ↔ response)
		// so the user can flip between halves of an exchange without backing
		// out to the list. In list mode these aren't bound to anything.
		if m.debugMode == debugDetail {
			if p, ok := debug.Pair(m.debugSelected); ok {
				m.debugSelected = p.ID
				m.refreshDebugContent()
			}
			return m, nil
		}

	case tea.KeyHome:
		if m.debugMode == debugList {
			if events := debug.Snapshot(); len(events) > 0 {
				m.debugSelected = events[0].ID
				m.refreshDebugContent()
			}
			return m, nil
		}

	case tea.KeyEnd:
		if m.debugMode == debugList {
			if events := debug.Snapshot(); len(events) > 0 {
				m.debugSelected = events[len(events)-1].ID
				m.refreshDebugContent()
			}
			return m, nil
		}
	}

	// Anything else (PgUp/PgDn, mouse wheel) feeds the viewport so the user
	// can scroll within a long detail payload or list.
	var cmd tea.Cmd
	m.debugView, cmd = m.debugView.Update(msg)
	return m, cmd
}

// moveSelection walks the snapshot to find the current selection, then
// shifts it by delta (clamped). The snapshot is small (≤500) so a linear
// scan is fine; the alternative is tracking an index that drifts as the
// ring rolls.
func (m *harness) moveSelection(delta int) {
	events := debug.Snapshot()
	if len(events) == 0 {
		return
	}
	idx := len(events) - 1 // default to latest if current selection is gone
	for i, e := range events {
		if e.ID == m.debugSelected {
			idx = i
			break
		}
	}
	idx += delta
	if idx < 0 {
		idx = 0
	}
	if idx >= len(events) {
		idx = len(events) - 1
	}
	m.debugSelected = events[idx].ID
}

// mouseSelect maps a left-click Y coordinate onto the debug panel and, if it
// lands on an event row, focuses the panel and selects that event.
func (m harness) mouseSelect(msg tea.MouseMsg) tea.Model {
	// Compute the y-range of the debug panel inside the alt-screen.
	debugTop := m.viewport.Height + 1                  // +1 for the panel's top border row
	debugBottom := debugTop + (debugPanelHeight - 2)   // -2 for top+bottom border
	if msg.Y < debugTop || msg.Y >= debugBottom {
		return m
	}
	row := msg.Y - debugTop + m.debugView.YOffset
	events := debug.Snapshot()
	if row < 0 || row >= len(events) {
		return m
	}
	if !m.debugFocus {
		m.input.Blur()
		m.debugFocus = true
	}
	m.debugSelected = events[row].ID
	// One click selects; pressing Enter (or another click in v2) drills in.
	m.debugMode = debugList
	m.refreshDebugContent()
	return m
}

// formatDebugEvent renders one event as a single line, color-coded by level
// and source. The leading `#<id>` is what the user passes to /debug show.
// A trailing dot marks events that have an inspectable payload.
func formatDebugEvent(e debug.Event) string {
	id := fmt.Sprintf("#%-4d", e.ID)
	ts := e.Time.Format("15:04:05.000")
	source := fmt.Sprintf("%-10s", e.Source)
	switch e.Source {
	case "provider":
		source = lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Render(source)
	case "tool":
		source = lipgloss.NewStyle().Foreground(lipgloss.Color("114")).Render(source)
	case "compact":
		source = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Render(source)
	default:
		if strings.HasPrefix(e.Source, "tool/") {
			source = lipgloss.NewStyle().Foreground(lipgloss.Color("141")).Render(source)
		}
	}
	msg := e.Message
	switch e.Level {
	case debug.LevelError:
		msg = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Render(msg)
	case debug.LevelWarn:
		msg = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(msg)
	}
	marker := " "
	if e.Payload != "" {
		marker = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Render("•")
	}
	line := Dimmed(id) + " " + Dimmed(ts) + " " + marker + " " + source + " " + msg
	// A response carries its CorrelatedID; surface it inline so the user
	// can read the conversation flow without entering detail mode.
	if e.CorrelatedID != 0 {
		line += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("141")).Render(
			fmt.Sprintf("↩ #%d", e.CorrelatedID))
	}
	return line
}

// ── view ──────────────────────────────────────────────────────────────────

// promptMarkerStyle and promptTextStyle echo the user's submitted prompt in
// scrollback. Bold cyan ❯ + plain text makes the line jump out against
// dimmed [tool] logs and the spinner's status line.
var promptMarkerStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("36")).Bold(true)

var promptTextStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("253")).Bold(true)

// turnDividerStyle and turnTimestampStyle render the "── 15:23:45 ──"
// header that sits above each user prompt and serves as a visual turn
// boundary in the scrollback.
var turnDividerStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("240"))

var turnTimestampStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("245")).Bold(true)

// replaceBanner regenerates the banner with the current shine column and
// splices it into place inside m.output. Anything written after the banner
// (the user's first prompt, agent output, etc.) is preserved — replaceBanner
// only rewrites the leading byte range that bannerEnd points at. Width
// comes from m.bannerWidth (captured before the stdout redirect) rather
// than a fresh TermWidth() call.
func (m *harness) replaceBanner() {
	post := ""
	full := m.output.String()
	if m.bannerEnd > 0 && m.bannerEnd <= len(full) {
		post = full[m.bannerEnd:]
	}
	animated := AnimatedBanner(m.bannerWidth, m.shineCol)
	m.output.Reset()
	m.output.WriteString(animated)
	m.bannerEnd = m.output.Len()
	m.output.WriteString(post)
}

// setConversationContent pushes the current scrollback buffer into the
// viewport, wrapping it to the viewport's width first so long lines
// (file output, JSON in errors, etc.) snap to the panel instead of
// trailing off the right edge. The wrap is ANSI-aware via lipgloss, so
// turn dividers, tool logs, and the styled ❯ prompt keep their colors
// after the linebreak.
//
// The banner portion is special-cased: it's already sized correctly and
// uses dense per-cell ANSI escapes for the shine animation, which lipgloss
// can collapse during its wrap pass. We pass the banner through unchanged
// and only wrap what comes after it.
func (m *harness) setConversationContent() {
	w := m.viewport.Width
	content := m.output.String()
	if w <= 0 {
		m.viewport.SetContent(content)
		return
	}
	if m.bannerEnd > 0 && m.bannerEnd <= len(content) {
		banner := content[:m.bannerEnd]
		rest := content[m.bannerEnd:]
		if rest != "" {
			rest = lipgloss.NewStyle().Width(w).Render(rest)
		}
		m.viewport.SetContent(banner + rest)
		return
	}
	m.viewport.SetContent(lipgloss.NewStyle().Width(w).Render(content))
}

// renderTurnHeader returns a "── 15:23:45 ─────────…" divider sized to
// width. Called when the user submits a new prompt so each turn has an
// unmistakable boundary in the scrollback.
func renderTurnHeader(width int) string {
	if width < 24 {
		width = 80
	}
	leftSep := "── "
	label := " " + time.Now().Format("15:04:05") + " "
	// lipgloss.Width handles wide characters / ANSI sanely.
	used := lipgloss.Width(leftSep) + lipgloss.Width(label)
	tail := width - used
	if tail < 3 {
		tail = 3
	}
	return turnDividerStyle.Render(leftSep) +
		turnTimestampStyle.Render(label) +
		turnDividerStyle.Render(strings.Repeat("─", tail))
}

var debugBoxStyle = lipgloss.NewStyle().
	Border(lipgloss.NormalBorder()).
	BorderForeground(lipgloss.Color("240")).
	Padding(0, 1)

var debugBoxFocusedStyle = debugBoxStyle.
	BorderForeground(lipgloss.Color("36"))

var selectedLineStyle = lipgloss.NewStyle().
	Background(lipgloss.Color("237"))

func (m harness) View() string {
	if m.state == stateAwaitingApproval && m.approvalDetail != "" {
		return m.viewApprovalModal()
	}
	if m.debugMode == debugDetail && debug.Enabled() {
		return m.viewDebugModal()
	}
	top := m.viewport.View()
	if m.agentDetail {
		top = m.viewAgentPane()
	}
	if m.agentsVisible() {
		top = lipgloss.JoinHorizontal(lipgloss.Top, top, m.viewSidebar(m.viewport.Height))
	}
	parts := []string{top}
	if debug.Enabled() {
		boxStyle := debugBoxStyle
		if m.debugFocus {
			boxStyle = debugBoxFocusedStyle
		}
		parts = append(parts, boxStyle.Width(max(m.width-2, 20)).Render(m.debugView.View()))
	}
	parts = append(parts, m.inputArea())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// viewApprovalModal renders the full-screen modal that takes over while
// the user is reviewing a write_file proposal. Same shape as the debug
// modal but with a yellow border so it's unmistakably a "you need to
// decide" state, and a different hint line (y/n instead of esc/tab).
func (m harness) viewApprovalModal() string {
	modalW, modalH := m.modalSize()

	titleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(strings.Repeat("─", max(modalW-4, 1)))

	inner := titleStyle.Render(m.approvalPrompt) + "\n" + sep + "\n" + m.debugView.View()

	modal := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("220")).
		Padding(0, 1).
		Width(modalW).
		Height(modalH).
		Render(inner)

	hint := Dimmed("  y: approve · n / esc: deny · pgup/pgdn / ↑↓: scroll")
	modalLine := lipgloss.PlaceHorizontal(m.width, lipgloss.Center, modal)
	hintLine := lipgloss.PlaceHorizontal(m.width, lipgloss.Center, hint)
	content := modalLine + "\n" + hintLine

	pad := (m.height - lipgloss.Height(content)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat("\n", pad) + content
}

// viewDebugModal renders the full-screen modal that takes over while the
// user is inspecting one event. The conversation viewport and input box are
// hidden — Esc returns to the panel, Tab goes straight back to the input.
func (m harness) viewDebugModal() string {
	modalW, modalH := m.modalSize()

	var titleText, pairHint string
	hasPair := false
	if e, ok := debug.FindByID(m.debugSelected); ok {
		titleText = fmt.Sprintf("#%d  %s  %s  %s",
			e.ID,
			e.Time.Format("15:04:05.000"),
			e.Source,
			e.Message)
		if p, pok := debug.Pair(e.ID); pok {
			hasPair = true
			arrow := "→ response"
			if e.CorrelatedID != 0 {
				arrow = "← request"
			}
			pairHint = fmt.Sprintf("   %s #%d", arrow, p.ID)
		}
	} else {
		titleText = fmt.Sprintf("#%d  (event no longer in the ring)", m.debugSelected)
	}

	titleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("36")).Bold(true)
	pairStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("141")).Bold(true)
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
		Render(strings.Repeat("─", max(modalW-4, 1)))

	titleLine := titleStyle.Render(titleText)
	if pairHint != "" {
		titleLine += pairStyle.Render(pairHint)
	}
	inner := titleLine + "\n" + sep + "\n" + m.debugView.View()

	modal := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("36")).
		Padding(0, 1).
		Width(modalW).
		Height(modalH).
		Render(inner)

	hintText := "  esc: close · pgup/pgdn: scroll · home/end: top/bottom · tab: input"
	if hasPair {
		hintText += " · ←/→: jump to pair"
	}
	hint := Dimmed(hintText)

	modalLine := lipgloss.PlaceHorizontal(m.width, lipgloss.Center, modal)
	hintLine := lipgloss.PlaceHorizontal(m.width, lipgloss.Center, hint)
	content := modalLine + "\n" + hintLine

	// Vertically center the modal in the alt-screen so it feels overlaid.
	pad := (m.height - lipgloss.Height(content)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat("\n", pad) + content
}

// activeSubagentSummary returns a short string like " · research" or
// " · research ×2, codereview" when subagents are running. Empty otherwise.
// Tacked onto the spinner line so the user can see what's in flight.
func activeSubagentSummary() string {
	active := subagent.Active()
	if len(active) == 0 {
		return ""
	}
	parts := make([]string, 0, len(active))
	for name, n := range active {
		if n == 1 {
			parts = append(parts, name)
		} else {
			parts = append(parts, fmt.Sprintf("%s ×%d", name, n))
		}
	}
	return " · " + strings.Join(parts, ", ")
}

func (m harness) inputArea() string {
	w := m.width - 2
	if w < 20 {
		w = 20
	}
	if m.state == stateAwaitingApproval {
		prompt := lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true).Render(" ? ")
		box := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("220")).
			Padding(0, 1).
			Width(w).
			Render(prompt + m.approvalPrompt + Dimmed(" (y/n)"))
		hint := Dimmed("  y: yes · n / esc / enter: no")
		return box + "\n" + hint
	}

	inputBorder := lipgloss.Color("36")
	if m.debugFocus || m.agentsFocus {
		// Dim the input border when the debug panel has focus so it's
		// obvious where keystrokes are going.
		inputBorder = lipgloss.Color("240")
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(inputBorder).
		Padding(0, 1).
		Width(w).
		Render(m.input.View())
	hint := Dimmed(m.hintText())

	// One line above the input box is always reserved. Priority order:
	//   running agent → spinner + thinking message
	//   MCP loading   → spinner + "connecting MCP…" while servers connect
	//   idle          → token-usage summary, if any
	statusLine := ""
	switch {
	case m.state == stateRunning:
		statusLine = "  " + m.spinner.View() + " " + Dimmed("thinking..."+activeSubagentSummary())
	case m.mcpLoading:
		statusLine = "  " + m.spinner.View() + " " + Dimmed(m.mcpStatusText())
	case m.state == stateIdle:
		statusLine = "  " + Dimmed(m.usageStatus())
	}
	return statusLine + "\n" + box + "\n" + hint
}

// mcpStatusText formats the loading line shown next to the spinner while
// background MCP servers are being connected. Empty when nothing's in
// flight (the View() switch checks mcpLoading before calling).
func (m harness) mcpStatusText() string {
	var msg string
	if m.mcpCurrent != "" {
		msg = fmt.Sprintf("connecting MCP: %s (%d/%d)", m.mcpCurrent, m.mcpDone+1, m.mcpTotal)
	} else if m.mcpDone < m.mcpTotal {
		msg = fmt.Sprintf("connecting MCP (%d/%d)", m.mcpDone, m.mcpTotal)
	} else {
		msg = fmt.Sprintf("MCP ready (%d server%s)", m.mcpDone-m.mcpFailed, plural(m.mcpDone-m.mcpFailed))
	}
	if m.mcpFailed > 0 {
		msg += fmt.Sprintf(" · %d failed", m.mcpFailed)
	}
	return msg
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// hintText returns the keymap hint shown under the input box. Changes with
// the active focus so the user always sees which keys are live.
func (m harness) hintText() string {
	if m.debugFocus {
		if m.debugMode == debugDetail {
			return "  debug · esc: back · pgup/pgdn: scroll · tab: input · ctrl-d: exit"
		}
		return "  debug · ↑/↓ home/end: select · enter: detail · esc/tab: input · ctrl-d: exit"
	}
	if m.agentDetail {
		return "  agent view · ↑/↓: switch agent · pgup/pgdn/wheel: scroll · m: merge · x: discard · esc: back · tab: input"
	}
	if m.agentsFocus {
		return "  agents · ↑/↓: select · enter/click: view · m: merge · x: discard · esc/tab: input · ctrl-d: exit"
	}
	if m.agentsVisible() {
		return "  enter: send · tab or click: agents panel · pgup/pgdn: scroll · ctrl-d: exit"
	}
	if debug.Enabled() {
		return "  enter: send · tab: debug panel · pgup/pgdn: scroll · ctrl-d: exit"
	}
	return "  enter: send · pgup/pgdn: scroll · ctrl-d: exit"
}

// usageStatus formats the session's cumulative token usage for the status
// line. Empty when no calls have been made or no UsageFunc was wired.
func (m harness) usageStatus() string {
	if m.usageFunc == nil {
		return ""
	}
	u, cost := m.usageFunc()
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return ""
	}
	parts := []string{
		fmt.Sprintf("%s in", formatThousands(u.InputTokens)),
		fmt.Sprintf("%s out", formatThousands(u.OutputTokens)),
	}
	if u.CacheCreationTokens > 0 || u.CacheReadTokens > 0 {
		parts = append(parts, fmt.Sprintf("cache %s/%s",
			formatThousands(u.CacheCreationTokens),
			formatThousands(u.CacheReadTokens)))
	}
	if cost >= 0 {
		parts = append(parts, fmt.Sprintf("~$%.4f", cost))
	}
	return strings.Join(parts, " · ")
}

// formatThousands inserts thousands separators into a non-negative int.
// Mirrors the helper in commands.go; lives here too to avoid a UI → main
// dependency.
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatThousands(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
}

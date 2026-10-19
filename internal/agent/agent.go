// Package agent encapsulates the agent loop. The root REPL holds one
// Agent; subagents (spawned by the delegate tool) are additional Agents
// with their own state, system prompt, and tool subset.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/betta-tech/byo-coding-agent/internal/api"
	"github.com/betta-tech/byo-coding-agent/internal/compact"
	"github.com/betta-tech/byo-coding-agent/internal/debug"
	"github.com/betta-tech/byo-coding-agent/internal/provider"
	"github.com/betta-tech/byo-coding-agent/internal/tool"
)

// Agent owns one conversation: a provider, a tool registry, a compaction
// strategy, and a slice of messages. The same struct serves the root REPL
// and any subagents.
type Agent struct {
	Name      string                           // shown in the spinner label; "" for root
	Provider  provider.Provider                //
	Tools     *tool.Registry                   // curated registry for this agent
	Compactor compact.CompactionStrategy       //
	System    string                           // system prompt
	MaxTurns  int                              // hard cap on tool-use iterations
	Verbose   bool                             // print compaction before/after
	LogPrefix string                           // prefix for tool-call log lines
	Quiet     bool                             // suppress assistant-text printing (set for subagents)
	Confirm   func(prompt, detail string) bool // nil = auto-approve every tool call. detail is optional long-form (e.g. a diff)
	Trusted   map[string]bool                  // tool names that skip Confirm (e.g. file tools confined to a worktree)
	Out       io.Writer                        // where logs and assistant text go; nil = os.Stdout

	// Optional observers around each tool call. Background agents use
	// them to drive the agents sidebar and the per-agent timeline view.
	OnToolCall   func(name, input string)
	OnToolResult func(name, result string, isErr bool, elapsed time.Duration)

	messages []api.Message
}

func New(p provider.Provider, system string, tools *tool.Registry) *Agent {
	return &Agent{
		Provider:  p,
		Tools:     tools,
		System:    system,
		Compactor: compact.NoCompaction{},
		MaxTurns:  20,
	}
}

// Messages returns the agent's conversation slice. Callers (e.g. /compact)
// may read it, but should not mutate it directly — use SetMessages instead.
func (a *Agent) Messages() []api.Message { return a.messages }

// SetMessages replaces the conversation slice. Used by /compact to write
// back the result of an ad-hoc strategy.
func (a *Agent) SetMessages(m []api.Message) { a.messages = m }

// ClearMessages wipes the conversation history.
func (a *Agent) ClearMessages() { a.messages = a.messages[:0] }

// Send appends a user message and runs the tool-use loop until the model
// stops requesting tools (or MaxTurns is reached). Returns the final
// assistant text concatenation, which is printed live as it arrives for
// the root agent and silently accumulated for subagents.
func (a *Agent) Send(ctx context.Context, prompt string) (string, error) {
	a.messages = append(a.messages, api.Message{
		Role:    api.RoleUser,
		Content: []api.Block{{Type: api.BlockText, Text: prompt}},
	})
	return a.loop(ctx)
}

func (a *Agent) loop(ctx context.Context) (string, error) {
	var finalText strings.Builder

	for turn := 0; turn < a.MaxTurns; turn++ {
		before := a.messages
		if compacted, err := a.Compactor.Compact(ctx, before); err != nil {
			fmt.Fprintf(a.out(), "%scompaction error: %v (continuing without)\n", a.LogPrefix, err)
			debug.Recordp("compact", debug.LevelError,
				fmt.Sprintf("%s: %v", a.compactorName(), err),
				api.RenderTranscript(before))
		} else {
			if len(compacted) != len(before) {
				summary := fmt.Sprintf("%s: %d → %d msgs", a.compactorName(), len(before), len(compacted))
				payload := "--- before (" + fmt.Sprint(len(before)) + " msgs) ---\n" +
					api.RenderTranscript(before) +
					"\n--- after (" + fmt.Sprint(len(compacted)) + " msgs) ---\n" +
					api.RenderTranscript(compacted)
				debug.Recordp("compact", debug.LevelInfo, summary, payload)
			}
			if a.Verbose && len(compacted) != len(before) {
				fmt.Fprintf(a.out(), "--- compaction: %d → %d ---\n", len(before), len(compacted))
				fmt.Fprint(a.out(), api.RenderTranscript(before))
				fmt.Fprintln(a.out(), "---")
				fmt.Fprint(a.out(), api.RenderTranscript(compacted))
				fmt.Fprintln(a.out(), "---")
			}
			a.messages = compacted
		}

		resp, err := a.Provider.Send(ctx, a.messages, a.Tools.Definitions())
		if err != nil {
			return finalText.String(), err
		}

		a.messages = append(a.messages, api.Message{Role: api.RoleAssistant, Content: resp.Content})

		var toolResults []api.Block
		var hasToolCall bool

		for _, b := range resp.Content {
			switch b.Type {
			case api.BlockText:
				if b.Text == "" {
					continue
				}
				if !a.Quiet {
					fmt.Fprintln(a.out(), b.Text)
				}
				finalText.WriteString(b.Text)
				finalText.WriteString("\n")
			case api.BlockToolUse:
				hasToolCall = true
				result, isErr := a.executeTool(ctx, b.ToolName, b.ToolInput)
				toolResults = append(toolResults, api.Block{
					Type:       api.BlockToolResult,
					ToolUseID:  b.ToolUseID,
					ToolResult: result,
					IsError:    isErr,
				})
			}
		}

		if resp.StopReason != api.StopToolUse || !hasToolCall {
			return strings.TrimSpace(finalText.String()), nil
		}

		a.messages = append(a.messages, api.Message{Role: api.RoleUser, Content: toolResults})
	}
	return strings.TrimSpace(finalText.String()), fmt.Errorf("max turns (%d) reached", a.MaxTurns)
}

// executeTool prints the tool-call line, optionally asks for confirmation,
// and dispatches to the registry.
func (a *Agent) executeTool(ctx context.Context, name, rawInput string) (string, bool) {
	fmt.Fprintf(a.out(), "%s[tool] %s %s\n", a.LogPrefix, name, rawInput)
	if a.OnToolCall != nil {
		a.OnToolCall(name, rawInput)
	}
	src := "tool"
	if a.Name != "" {
		src = "tool/" + a.Name
	}
	reqID := debug.Recordp(src, debug.LevelInfo,
		fmt.Sprintf("→ %s %s", name, truncate(rawInput, 200)),
		rawInput)

	// Build the approval prompt. For write_file we additionally compute a
	// unified diff against the current file contents; the UI shows that
	// in a modal so the user can review the change before approving. The
	// prompt names the call because background agents' [tool] lines go to
	// their own log, not the scrollback — the prompt is all the user sees.
	needsConfirm := a.Confirm != nil && !a.Trusted[name]
	prompt, detail := fmt.Sprintf("approve %s %s?", name, truncate(rawInput, 120)), ""
	if needsConfirm && name == "write_file" {
		if d := buildWriteDiff(rawInput); d != "" {
			detail = d
			if path := extractWritePath(rawInput); path != "" {
				prompt = "approve write to " + path + "?"
			} else {
				prompt = "approve write?"
			}
		}
	}

	if needsConfirm && !a.Confirm(prompt, detail) {
		debug.Recordfc(reqID, src, debug.LevelWarn, "denied: %s", name)
		if a.OnToolResult != nil {
			a.OnToolResult(name, "user denied this tool call", true, 0)
		}
		return "user denied this tool call", true
	}

	start := time.Now()
	result, isErr := a.Tools.Execute(ctx, name, rawInput)
	elapsed := time.Since(start).Round(time.Millisecond)
	level := debug.LevelInfo
	verdict := fmt.Sprintf("← %s (%v, %d bytes)", name, elapsed, len(result))
	if isErr {
		level = debug.LevelError
		verdict = fmt.Sprintf("← %s error (%v): %s", name, elapsed, truncate(result, 200))
	}
	debug.Recordpc(reqID, src, level, verdict, result)
	if a.OnToolResult != nil {
		a.OnToolResult(name, result, isErr, elapsed)
	}
	return result, isErr
}

// out is a method rather than a default set in New because main swaps
// os.Stdout for a pipe *after* the root agent is constructed.
func (a *Agent) out() io.Writer {
	if a.Out != nil {
		return a.Out
	}
	return os.Stdout
}

// compactorName returns a short label for whichever compaction strategy is
// active. Used in debug-log entries so it's obvious what ran.
func (a *Agent) compactorName() string {
	if a.Compactor == nil {
		return "none"
	}
	return fmt.Sprintf("%T", a.Compactor)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractWritePath pulls the path out of a write_file tool input so the
// approval prompt can name the target file. Returns "" on any parse
// failure — the caller falls back to a generic prompt.
func extractWritePath(rawInput string) string {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(rawInput), &in); err != nil {
		return ""
	}
	return in.Path
}

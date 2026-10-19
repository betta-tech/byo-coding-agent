package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/betta-tech/byo-coding-agent/internal/api"
	"github.com/betta-tech/byo-coding-agent/internal/compact"
	"github.com/betta-tech/byo-coding-agent/internal/debug"
	"github.com/betta-tech/byo-coding-agent/internal/mcp"
	"github.com/betta-tech/byo-coding-agent/internal/subagent"
	"github.com/betta-tech/byo-coding-agent/internal/ui"
)

type command struct {
	description string
	usage       string // optional; falls back to "/name"
	run         func(args string)
}

var commands = map[string]command{}

func init() {
	commands["help"] = command{description: "show available commands", run: cmdHelp}
	commands["provider"] = command{description: "show or change the LLM provider", usage: "/provider [anthropic|openai]", run: cmdProvider}
	commands["model"] = command{description: "show or change the model", usage: "/model [name]", run: cmdModel}
	commands["clear"] = command{description: "clear conversation history", run: cmdClear}
	commands["tools"] = command{description: "list available tools", run: cmdTools}
	commands["subagents"] = command{description: "list subagents (registered and currently running)", run: cmdSubagents}
	commands["compact"] = command{
		description: "run compaction now (optionally with a specific strategy)",
		usage:       "/compact [sliding|summarize|none]",
		run:         cmdCompact,
	}
	commands["verbose"] = command{
		description: "toggle printing of compaction before/after",
		usage:       "/verbose [on|off]",
		run:         cmdVerbose,
	}
	commands["tokens"] = command{description: "show cumulative token usage and estimated cost", run: cmdTokens}
	commands["debug"] = command{
		description: "control the debug event panel (toggle / inspect entries)",
		usage:       "/debug [on|off|clear|ls|show [id]]",
		run:         cmdDebug,
	}
	commands["agents"] = command{
		description: "list background agents, or print one agent's log",
		usage:       "/agents [log <name>]",
		run:         cmdAgents,
	}
	commands["merge"] = command{description: "merge a finished background agent's branch", usage: "/merge <name>", run: cmdMerge}
	commands["discard"] = command{description: "stop a background agent and delete its worktree", usage: "/discard <name>", run: cmdDiscard}
	commands["exit"] = command{description: "exit the harness", run: cmdExit}
}

// tokenReporter is what cmdTokens type-asserts to. We didn't widen the
// Provider interface because not every backend has a "tokens" concept; any
// provider that does just satisfies this implicit contract.
type tokenReporter interface {
	TotalUsage() api.Usage
	EstimatedCostUSD() float64
}

func cmdTokens(_ string) {
	stats, ok := rootAgent.Provider.(tokenReporter)
	if !ok {
		fmt.Println(ui.Dimmed("this provider doesn't report token usage"))
		return
	}
	u := stats.TotalUsage()
	cost := stats.EstimatedCostUSD()
	fmt.Println(ui.Dimmed("session usage:"))
	fmt.Printf("  input          %s\n", ui.Cyan(formatThousands(u.InputTokens)))
	fmt.Printf("  output         %s\n", ui.Cyan(formatThousands(u.OutputTokens)))
	if u.CacheCreationTokens > 0 || u.CacheReadTokens > 0 {
		fmt.Printf("  cache write    %s\n", ui.Cyan(formatThousands(u.CacheCreationTokens)))
		fmt.Printf("  cache read     %s\n", ui.Cyan(formatThousands(u.CacheReadTokens)))
	}
	if cost >= 0 {
		fmt.Printf("  est. cost      %s\n", ui.Cyan(fmt.Sprintf("$%.4f", cost)))
	} else {
		fmt.Printf("  est. cost      %s\n", ui.Dimmed("(unknown model — no rate)"))
	}
}

// formatThousands inserts thousands separators into a non-negative int.
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatThousands(n/1000) + "," + fmt.Sprintf("%03d", n%1000)
}

// runCommand handles a line that starts with "/". Returns true if handled,
// false if the line should fall through to the model as a normal message.
func runCommand(line string) bool {
	if !strings.HasPrefix(line, "/") {
		return false
	}
	parts := strings.SplitN(strings.TrimPrefix(line, "/"), " ", 2)
	name := parts[0]
	args := ""
	if len(parts) > 1 {
		args = strings.TrimSpace(parts[1])
	}
	c, ok := commands[name]
	if !ok {
		fmt.Printf("unknown command: /%s (try /help)\n", name)
		return true
	}
	c.run(args)
	return true
}

func cmdHelp(_ string) {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println(ui.Dimmed("available commands:"))
	for _, n := range names {
		c := commands[n]
		display := "/" + n
		if c.usage != "" {
			display = c.usage
		}
		fmt.Printf("  %-22s %s\n", display, ui.Dimmed(c.description))
	}
}

// knownModels are suggestions shown by /model when no model is passed.
// Keyed by provider short name (see ProviderKind in main.go). Any model id
// can be set — validation happens at the next Send call.
var knownModels = map[string][]string{
	"anthropic": {
		"claude-opus-4-7",
		"claude-opus-4-6",
		"claude-sonnet-4-6",
		"claude-haiku-4-5",
	},
	"openai": {
		"gpt-5-codex",
		"gpt-5",
		"gpt-5-mini",
		"gpt-4o",
		"gpt-4o-mini",
		"o3-mini",
	},
}

func cmdModel(args string) {
	kind := ProviderKind(rootAgent.Provider)
	if args == "" {
		fmt.Printf("current: %s  %s\n", ui.Cyan(rootAgent.Provider.Model()), ui.Dimmed("("+kind+")"))
		if models, ok := knownModels[kind]; ok {
			fmt.Println(ui.Dimmed("suggestions:"))
			for _, m := range models {
				fmt.Printf("  %s\n", m)
			}
		}
		fmt.Println(ui.Dimmed("(or pass any model id — validated on next call)"))
		return
	}
	rootAgent.Provider.SetModel(args)
	fmt.Printf("model: %s\n", ui.Cyan(args))
}

func cmdProvider(args string) {
	if args == "" {
		fmt.Printf("current: %s  %s\n",
			ui.Cyan(ProviderKind(rootAgent.Provider)),
			ui.Dimmed("(model: "+rootAgent.Provider.Model()+")"))
		fmt.Println(ui.Dimmed("choices:"))
		for _, p := range KnownProviders {
			fmt.Printf("  %s\n", p)
		}
		fmt.Println(ui.Dimmed("usage: /provider <name> [model]"))
		return
	}
	// Optional second token = model id to set on the new provider.
	parts := strings.Fields(args)
	name := parts[0]
	model := ""
	if len(parts) > 1 {
		model = parts[1]
	}
	if err := switchProvider(name, model); err != nil {
		fmt.Printf("provider: %v\n", err)
		return
	}
	fmt.Printf("provider: %s  %s\n",
		ui.Cyan(ProviderKind(rootAgent.Provider)),
		ui.Dimmed("(model: "+rootAgent.Provider.Model()+")"))
	if !apiKeySet(name) {
		fmt.Println(ui.Dimmed("warning: " + apiKeyEnv(name) + " is not set; the next call will fail"))
	}
}

func apiKeySet(provider string) bool {
	return os.Getenv(apiKeyEnv(provider)) != ""
}

func apiKeyEnv(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	default:
		return ""
	}
}

func cmdClear(_ string) {
	rootAgent.ClearMessages()
	fmt.Println(ui.Dimmed("conversation cleared"))
}

func cmdTools(_ string) {
	fmt.Println(ui.Dimmed("tools available:"))
	for _, def := range rootAgent.Tools.Definitions() {
		origin := ""
		// Type-assert the registered Tool to detect MCP-backed ones. Local
		// Go tools won't satisfy *mcp.MCPTool, so the marker only appears
		// next to tools that live behind a remote server.
		if t, ok := rootAgent.Tools.Get(def.Name); ok {
			if _, isMCP := t.(*mcp.MCPTool); isMCP {
				origin = " " + ui.Dimmed("(mcp)")
			}
		}
		fmt.Printf("  %s%s  %s\n", ui.Cyan(def.Name), origin, ui.Dimmed(def.Description))
	}
}

// cmdSubagents lists registered subagent types and, separately, any that
// are currently running. The active list is empty when nothing is in flight.
func cmdSubagents(_ string) {
	all := subagent.Default.All()
	if len(all) == 0 {
		fmt.Println(ui.Dimmed("no subagents registered"))
	} else {
		fmt.Println(ui.Dimmed("registered subagents:"))
		for _, s := range all {
			fmt.Printf("  %s  %s\n", ui.Cyan(s.Name()), ui.Dimmed(s.Description()))
		}
	}

	active := subagent.Active()
	if len(active) == 0 {
		fmt.Println(ui.Dimmed("currently running: (none)"))
		return
	}
	fmt.Println(ui.Dimmed("currently running:"))
	for name, n := range active {
		if n == 1 {
			fmt.Printf("  %s\n", ui.Cyan(name))
		} else {
			fmt.Printf("  %s ×%d\n", ui.Cyan(name), n)
		}
	}
}

func cmdCompact(args string) {
	strategy := rootAgent.Compactor
	switch strings.ToLower(args) {
	case "":
		// use the configured compactor
	case "sliding":
		strategy = &compact.SlidingWindow{KeepLast: 6}
	case "summarize":
		strategy = &compact.Summarize{Provider: rootAgent.Provider, Threshold: 0, KeepRecent: 4}
	case "none":
		strategy = compact.NoCompaction{}
	default:
		fmt.Printf("unknown strategy: %s (try sliding, summarize, or none)\n", args)
		return
	}

	before := rootAgent.Messages()
	compacted, err := strategy.Compact(context.Background(), before)
	if err != nil {
		fmt.Printf("compaction error: %v\n", err)
		return
	}
	rootAgent.SetMessages(compacted)
	fmt.Println(ui.Dimmed(fmt.Sprintf("compacted: %d → %d messages", len(before), len(compacted))))
	if rootAgent.Verbose && len(before) != len(compacted) {
		ui.PrintCompaction(before, compacted)
	}
}

func cmdVerbose(args string) {
	switch strings.ToLower(args) {
	case "":
		rootAgent.Verbose = !rootAgent.Verbose
	case "on", "true", "yes":
		rootAgent.Verbose = true
	case "off", "false", "no":
		rootAgent.Verbose = false
	default:
		fmt.Printf("unknown value: %s (try on/off)\n", args)
		return
	}
	state := "off"
	if rootAgent.Verbose {
		state = "on"
	}
	fmt.Printf("verbose: %s\n", state)
}

func cmdDebug(args string) {
	parts := strings.Fields(args)
	verb := ""
	rest := ""
	if len(parts) > 0 {
		verb = strings.ToLower(parts[0])
	}
	if len(parts) > 1 {
		rest = parts[1]
	}

	switch verb {
	case "":
		debug.SetEnabled(!debug.Enabled())
	case "on", "true", "yes":
		debug.SetEnabled(true)
	case "off", "false", "no":
		debug.SetEnabled(false)
	case "clear":
		debug.Clear()
		fmt.Println(ui.Dimmed("debug log cleared"))
		notifyDebugUI()
		return
	case "ls", "list":
		cmdDebugList()
		return
	case "show", "dump":
		cmdDebugShow(rest)
		return
	default:
		fmt.Printf("unknown value: %s (try on/off/clear/ls/show)\n", verb)
		return
	}
	state := "off"
	if debug.Enabled() {
		state = "on"
	}
	fmt.Printf("debug: %s\n", state)
	notifyDebugUI()
}

// cmdDebugList prints every event currently in the ring, one per line, with
// a marker for events that have an inspectable payload.
func cmdDebugList() {
	events := debug.Snapshot()
	if len(events) == 0 {
		fmt.Println(ui.Dimmed("debug log is empty"))
		return
	}
	for _, e := range events {
		marker := " "
		if e.Payload != "" {
			marker = "•"
		}
		fmt.Printf("  %s #%-4d %s %-10s %s\n",
			marker,
			e.ID,
			ui.Dimmed(e.Time.Format("15:04:05.000")),
			e.Source,
			e.Message)
	}
	fmt.Println(ui.Dimmed(fmt.Sprintf("%d events (• = has payload). use /debug show <id> for full content.", len(events))))
}

// cmdDebugShow dumps a single event's full payload to scrollback. With no
// argument it shows the most recent event that has a payload.
func cmdDebugShow(idStr string) {
	var (
		e  debug.Event
		ok bool
	)
	if idStr == "" {
		e, ok = debug.Latest()
		if !ok {
			fmt.Println(ui.Dimmed("debug log is empty"))
			return
		}
	} else {
		id, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			fmt.Printf("not a valid id: %s\n", idStr)
			return
		}
		e, ok = debug.FindByID(id)
		if !ok {
			fmt.Printf("no event with id #%d (it may have aged out of the ring)\n", id)
			return
		}
	}
	header := fmt.Sprintf("#%d  %s  %s  %s",
		e.ID, e.Time.Format("15:04:05.000"), e.Source, e.Message)
	fmt.Println(ui.Cyan(header))
	if e.Payload == "" {
		fmt.Println(ui.Dimmed("(no payload)"))
		return
	}
	// Reuse the modal's JSON-aware highlighter so /debug show in scrollback
	// reads as nicely as the modal does. No wrap (width=0) — let the
	// scrollback viewport do its own thing.
	fmt.Println(ui.HighlightPayload(e.Payload, 0))
}

func cmdExit(_ string) {
	os.Exit(0)
}

func cmdAgents(args string) {
	if name, ok := strings.CutPrefix(args, "log "); ok {
		log := subagent.Background.Log(strings.TrimSpace(name))
		if log == "" {
			fmt.Println(ui.Dimmed("no log for that agent"))
			return
		}
		fmt.Print(log)
		return
	}
	agents := subagent.Background.Snapshot()
	if len(agents) == 0 {
		fmt.Println(ui.Dimmed("no background agents yet — ask for parallel work and the model will spawn some"))
		return
	}
	for _, a := range agents {
		fmt.Printf("  %s  %-9s %s  %s  %s\n",
			ui.Cyan(fmt.Sprintf("%-14s", a.Name)),
			a.State,
			ui.Dimmed(fmt.Sprintf("+%d −%d", a.Added, a.Removed)),
			ui.Dimmed(a.Elapsed().Round(time.Second).String()),
			ui.Dimmed(a.Branch))
	}
}

func cmdMerge(name string) {
	if name == "" {
		fmt.Println("usage: /merge <name>")
		return
	}
	ui.MergeAgent(name)
}

func cmdDiscard(name string) {
	if name == "" {
		fmt.Println("usage: /discard <name>")
		return
	}
	ui.DiscardAgent(name)
}

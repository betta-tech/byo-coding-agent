package subagent

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/betta-tech/byo-coding-agent/internal/agent"
	"github.com/betta-tech/byo-coding-agent/internal/provider"
	"github.com/betta-tech/byo-coding-agent/internal/tool"
	"github.com/betta-tech/byo-coding-agent/internal/worktree"
)

// Background agents differ from delegate_* subagents in two ways: the
// spawning tool call returns immediately instead of blocking until the
// agent finishes, and each agent edits its own git worktree, so several
// of them can change the same files at once without stepping on each
// other. The user decides what lands via /merge or /discard.

// State is what the sidebar shows next to each agent.
type State int

const (
	StateWorking   State = iota
	StateBlocked         // waiting on the user to approve a tool call
	StateDone            // finished; branch ready to review
	StateFailed          // agent loop returned an error
	StateMerged          // branch merged into the main checkout
	StateDiscarded       // worktree and branch deleted
)

func (s State) String() string {
	return [...]string{"working", "blocked", "done", "failed", "merged", "discarded"}[s]
}

// Settled reports whether the agent has stopped running.
func (s State) Settled() bool { return s >= StateDone }

// Status is a point-in-time copy of one background agent, safe to read
// from the UI goroutine while the agent keeps running.
type Status struct {
	Name           string
	Task           string
	State          State
	Activity       string // latest tool call, e.g. "bash go test ./..."
	ToolCalls      int
	Added, Removed int
	Started, Ended time.Time
	Branch         string
	Path           string
	Result         string
	Err            error
}

// Elapsed is wall-clock time so far, frozen once the agent settles.
func (s Status) Elapsed() time.Duration {
	if s.Ended.IsZero() {
		return time.Since(s.Started)
	}
	return s.Ended.Sub(s.Started)
}

// Event is one entry in an agent's timeline: a tool call (with its result
// once it lands) or something the agent said. The raw log has the same
// information, but as JSON blobs; the timeline is what the agent view
// renders.
type Event struct {
	Time    time.Time
	Tool    string // "" for a text event
	Text    string // tool input (compacted) or the agent's message
	Result  string // tool output; empty until Done
	IsErr   bool
	Done    bool
	Elapsed time.Duration
}

type bgAgent struct {
	Status
	wt     *worktree.Worktree
	log    strings.Builder
	events []Event
	cancel context.CancelFunc
}

// Fleet owns every background agent spawned this session.
type Fleet struct {
	// Configured by main. Provider and Tools are re-set on /provider.
	Provider provider.Provider
	Tools    *tool.Registry
	Confirm  func(prompt, detail string) bool // UI approval; nil = auto-approve everything
	OnChange func()                           // fires on any visible change; drives UI refresh
	OnFinish func(Status)                     // fires once when an agent settles on its own

	mu     sync.Mutex
	agents []*bgAgent
	notes  []string // finish reports waiting to be handed to the root agent
}

// Background is the session's fleet.
var Background = &Fleet{}

var unsafeName = regexp.MustCompile(`[^a-z0-9-]+`)

// Spawn creates a worktree, starts an agent in it on a goroutine, and
// returns as soon as it's running.
func (f *Fleet) Spawn(name, task string) (Status, error) {
	f.mu.Lock()
	p, tools, confirm := f.Provider, f.Tools, f.Confirm
	f.mu.Unlock()
	if p == nil || tools == nil {
		return Status{}, fmt.Errorf("background agents are not configured")
	}

	repo, err := worktree.RepoRoot()
	if err != nil {
		return Status{}, err
	}
	name = strings.Trim(unsafeName.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if name == "" {
		name = "agent"
	}
	f.mu.Lock()
	base := name
	for i := 2; worktree.Exists(repo, name) || f.find(name) != nil; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	f.mu.Unlock()

	wt, err := worktree.Create(repo, name)
	if err != nil {
		return Status{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := &bgAgent{
		Status: Status{
			Name: name, Task: task, State: StateWorking, Started: time.Now(),
			Branch: wt.Branch, Path: wt.Path, Activity: "starting…",
		},
		wt:     wt,
		cancel: cancel,
	}

	a := agent.New(p, backgroundSystem(wt), tools)
	a.Name = name
	a.MaxTurns = 30
	a.Out = &fleetLog{f: f, rec: rec}
	// File tools are confined to the worktree by tool.ResolvePath, so a
	// bad write only costs a discarded branch. bash isn't confined, so it
	// still goes through the user — and the sidebar shows the agent as
	// blocked while it waits.
	a.Trusted = map[string]bool{"read_file": true, "write_file": true}
	if confirm != nil {
		a.Confirm = func(prompt, detail string) bool {
			f.setState(rec, StateBlocked)
			ok := confirm("["+name+"] "+prompt, detail)
			f.setState(rec, StateWorking)
			return ok
		}
	}
	a.OnToolCall = func(tool, input string) {
		f.mu.Lock()
		rec.events = append(rec.events, Event{Time: time.Now(), Tool: tool, Text: compactInput(input)})
		f.mu.Unlock()
		f.update(rec, func(s *Status) {
			s.ToolCalls++
			s.Activity = tool + " " + compactInput(input)
		})
	}
	// An agent runs its tools one at a time, so a result always belongs
	// to the most recent tool event.
	a.OnToolResult = func(_, result string, isErr bool, elapsed time.Duration) {
		f.mu.Lock()
		if n := len(rec.events); n > 0 {
			e := &rec.events[n-1]
			e.Result, e.IsErr, e.Done, e.Elapsed = result, isErr, true, elapsed
		}
		f.mu.Unlock()
		f.changed()
	}

	f.mu.Lock()
	f.agents = append(f.agents, rec)
	f.mu.Unlock()
	f.changed()

	go f.run(ctx, rec, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	return rec.Status, nil
}

func (f *Fleet) run(ctx context.Context, rec *bgAgent, a *agent.Agent) {
	done := make(chan struct{})
	go f.pollDiff(rec, done)

	result, err := a.Send(tool.WithWorkDir(ctx, rec.wt.Path), rec.Task)
	close(done)
	added, removed, _ := rec.wt.DiffStat()

	f.mu.Lock()
	if rec.State == StateDiscarded {
		// /discard cancelled us mid-run; nothing to report.
		f.mu.Unlock()
		return
	}
	rec.Added, rec.Removed = added, removed
	rec.Ended = time.Now()
	rec.Result = result
	rec.Err = err
	rec.State = StateDone
	rec.Activity = "ready to review"
	if err != nil {
		rec.State = StateFailed
		rec.Activity = err.Error()
	}
	f.notes = append(f.notes, finishNote(rec.Status))
	st := rec.Status
	onFinish := f.OnFinish
	f.mu.Unlock()

	f.changed()
	if onFinish != nil {
		onFinish(st)
	}
}

// pollDiff refreshes the +/- counter while the agent works, so the
// sidebar shows code piling up in real time.
func (f *Fleet) pollDiff(rec *bgAgent, done <-chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if a, r, err := rec.wt.DiffStat(); err == nil {
				f.update(rec, func(s *Status) { s.Added, s.Removed = a, r })
			}
		}
	}
}

// Snapshot returns every agent's status, in spawn order.
func (f *Fleet) Snapshot() []Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Status, len(f.agents))
	for i, a := range f.agents {
		out[i] = a.Status
	}
	return out
}

// Busy reports whether any agent is still running (drives the spinner).
func (f *Fleet) Busy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if !a.State.Settled() {
			return true
		}
	}
	return false
}

// Log returns everything the agent has printed: its tool calls and text.
func (f *Fleet) Log(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.find(name); a != nil {
		return a.log.String()
	}
	return ""
}

// Events returns a copy of the agent's timeline.
func (f *Fleet) Events(name string) []Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.find(name); a != nil {
		return append([]Event(nil), a.events...)
	}
	return nil
}

// DrainNotes hands pending finish reports to the caller exactly once.
// main prepends them to the user's next message, which is how the root
// agent finds out what its background agents did.
func (f *Fleet) DrainNotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.notes
	f.notes = nil
	return n
}

// Merge commits the agent's worktree, merges its branch into the main
// checkout, and cleans up the worktree.
func (f *Fleet) Merge(name string) (string, error) {
	f.mu.Lock()
	rec := f.find(name)
	if rec == nil {
		f.mu.Unlock()
		return "", fmt.Errorf("no background agent named %q", name)
	}
	if rec.State != StateDone && rec.State != StateFailed {
		st := rec.State
		f.mu.Unlock()
		return "", fmt.Errorf("%s is %s — only finished agents can be merged", name, st)
	}
	f.mu.Unlock()

	out, err := rec.wt.Merge(fmt.Sprintf("agent %s: %s", name, firstLine(rec.Task)))
	if err != nil {
		return out, err
	}
	if err := rec.wt.Remove(); err != nil {
		return out, fmt.Errorf("merged, but cleanup failed: %w", err)
	}
	f.setState(rec, StateMerged)
	return out, nil
}

// Discard stops the agent if it's still running and deletes its worktree
// and branch.
func (f *Fleet) Discard(name string) error {
	f.mu.Lock()
	rec := f.find(name)
	if rec == nil {
		f.mu.Unlock()
		return fmt.Errorf("no background agent named %q", name)
	}
	if rec.State == StateMerged || rec.State == StateDiscarded {
		st := rec.State
		f.mu.Unlock()
		return fmt.Errorf("%s is already %s", name, st)
	}
	rec.State = StateDiscarded
	rec.Activity = ""
	if rec.Ended.IsZero() {
		rec.Ended = time.Now()
	}
	f.mu.Unlock()

	rec.cancel()
	err := rec.wt.Remove()
	f.changed()
	return err
}

// find must be called with f.mu held.
func (f *Fleet) find(name string) *bgAgent {
	for _, a := range f.agents {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (f *Fleet) update(rec *bgAgent, fn func(*Status)) {
	f.mu.Lock()
	fn(&rec.Status)
	f.mu.Unlock()
	f.changed()
}

func (f *Fleet) setState(rec *bgAgent, s State) {
	f.update(rec, func(st *Status) {
		if st.State != StateDiscarded {
			st.State = s
		}
	})
}

func (f *Fleet) changed() {
	if f.OnChange != nil {
		f.OnChange()
	}
}

// fleetLog captures an agent's output into its own buffer instead of the
// shared scrollback — three agents printing into one stream is unreadable.
type fleetLog struct {
	f   *Fleet
	rec *bgAgent
}

func (l *fleetLog) Write(p []byte) (int, error) {
	l.f.mu.Lock()
	l.rec.log.Write(p)
	// The agent prints each message with one Fprintln, so a write is a
	// whole message. [tool] lines are already timeline events.
	if text := strings.TrimSpace(string(p)); text != "" && !strings.HasPrefix(text, "[tool]") {
		l.rec.events = append(l.rec.events, Event{Time: time.Now(), Text: text})
	}
	l.f.mu.Unlock()
	l.f.changed()
	return len(p), nil
}

func backgroundSystem(wt *worktree.Worktree) string {
	return fmt.Sprintf(`You are a background coding agent working in an isolated git worktree.

Worktree: %s
Branch:   %s

Other agents may be working on related tasks at the same time, each in their own worktree. You can't see their changes and they can't see yours.

Rules:
- Use paths relative to your worktree. bash already runs inside it; never cd out of it or touch absolute paths elsewhere.
- Make the change, then verify it (build, tests) when the project has a way to.
- Don't commit or switch branches. The user reviews your branch and merges it.
- You work alone. There is nobody to ask, so make reasonable decisions and note them.
- End with a short report: what you changed, how you verified it, and anything left open.`, wt.Path, wt.Branch)
}

func finishNote(s Status) string {
	outcome := "finished"
	if s.State == StateFailed {
		outcome = fmt.Sprintf("failed (%v)", s.Err)
	}
	result := s.Result
	if len(result) > 2000 {
		result = result[:2000] + "\n[...truncated...]"
	}
	return fmt.Sprintf("Background agent %q %s after %s: +%d −%d lines on branch %s. The user can /merge %s or /discard %s.\nIts report:\n%s",
		s.Name, outcome, s.Elapsed().Round(time.Second), s.Added, s.Removed, s.Branch, s.Name, s.Name, result)
}

// compactInput turns a tool's JSON input into something that fits on a
// sidebar line: for the common single-field tools, just the value.
func compactInput(raw string) string {
	for _, key := range []string{`"command":"`, `"path":"`} {
		if i := strings.Index(raw, key); i >= 0 {
			v := raw[i+len(key):]
			if j := strings.Index(v, `"`); j >= 0 {
				v = v[:j]
			}
			return v
		}
	}
	return raw
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 72 {
		s = s[:72] + "…"
	}
	return s
}

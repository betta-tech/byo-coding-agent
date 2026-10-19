package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/betta-tech/byo-coding-agent/internal/api"
	"github.com/betta-tech/byo-coding-agent/internal/subagent"
)

// SpawnTool starts a background agent and returns immediately — the
// non-blocking sibling of DelegateTool. The model calls it several times
// in one turn to fan work out; the agents run in parallel, each in its
// own git worktree, while the user keeps chatting. Lives in main for the
// same import-cycle reason as DelegateTool.
type SpawnTool struct{}

func (SpawnTool) Definition() api.ToolDef {
	return api.ToolDef{
		Name: "spawn_agent",
		Description: "Start a background coding agent in its own git worktree and return immediately. " +
			"Use it for independent work that can run in parallel: trying several approaches to the same " +
			"problem, or splitting a change into parts that touch different files. Call it several times in " +
			"one turn to run agents at once. Each agent can read, write, and run bash inside its worktree, " +
			"starting from the last commit (uncommitted changes are not visible to it). The user watches " +
			"progress in the agents sidebar and decides what to keep with /merge or /discard. You get a " +
			"report automatically when each agent finishes, so don't wait for it or poll.",
		InputSchema: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Short kebab-case label shown in the sidebar and used for the branch, e.g. \"cache-lru\".",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "Self-contained instructions. The agent can't see this conversation, so include file names, goals, and how to verify.",
			},
		},
		Required: []string{"name", "task"},
	}
}

func (SpawnTool) Execute(_ context.Context, rawInput string) (string, bool) {
	var in struct {
		Name string `json:"name"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(rawInput), &in); err != nil {
		return fmt.Sprintf("invalid tool input: %v", err), true
	}
	st, err := subagent.Background.Spawn(in.Name, in.Task)
	if err != nil {
		return fmt.Sprintf("could not start background agent: %v", err), true
	}
	return fmt.Sprintf("Started background agent %q on branch %s (worktree %s). It runs on its own; "+
		"you'll get its report with a later user message. Tell the user it's running and move on.",
		st.Name, st.Branch, st.Path), false
}

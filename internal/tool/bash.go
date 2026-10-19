package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/betta-tech/byo-coding-agent/internal/api"
)

type BashTool struct{}

func init() { Default.Register(&BashTool{}) }

func (BashTool) Definition() api.ToolDef {
	return api.ToolDef{
		Name:        "bash",
		Description: "Run a shell command. Returns combined stdout and stderr.",
		InputSchema: map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute.",
			},
		},
		Required: []string{"command"},
	}
}

func (BashTool) Execute(ctx context.Context, rawInput string) (string, bool) {
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(rawInput), &in); err != nil {
		return fmt.Sprintf("invalid tool input: %v", err), true
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", in.Command)
	cmd.Dir = WorkDir(ctx) // "" = process cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s\n[exit error: %v]", out, err), true
	}
	return string(out), false
}

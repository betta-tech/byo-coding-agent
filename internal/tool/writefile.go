package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/betta-tech/byo-coding-agent/internal/api"
)

type WriteFileTool struct{}

func init() { Default.Register(&WriteFileTool{}) }

func (WriteFileTool) Definition() api.ToolDef {
	return api.ToolDef{
		Name:        "write_file",
		Description: "Write content to a file at the given path. Creates or overwrites.",
		InputSchema: map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to write.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The content to write.",
			},
		},
		Required: []string{"path", "content"},
	}
}

func (WriteFileTool) Execute(ctx context.Context, rawInput string) (string, bool) {
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawInput), &in); err != nil {
		return fmt.Sprintf("invalid tool input: %v", err), true
	}
	path, err := ResolvePath(ctx, in.Path)
	if err != nil {
		return err.Error(), true
	}
	if err := os.WriteFile(path, []byte(in.Content), 0644); err != nil {
		return err.Error(), true
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), false
}

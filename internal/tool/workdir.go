package tool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Background agents each work in their own git worktree. The directory
// travels through ctx rather than os.Chdir because a process has exactly
// one cwd — three agents running at once can't each "be" somewhere else.
type workDirKey struct{}

// WithWorkDir scopes the file tools and bash to dir for every tool call
// made with the returned context. dir should be absolute.
func WithWorkDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, workDirKey{}, dir)
}

// WorkDir returns the directory set by WithWorkDir, or "" for the root
// agent, which works in the process cwd.
func WorkDir(ctx context.Context) string {
	dir, _ := ctx.Value(workDirKey{}).(string)
	return dir
}

// ResolvePath anchors relative paths at the ctx's work dir and refuses
// paths that land outside it, so one agent can't write into another's
// worktree (or the user's checkout). bash can still escape — this is a
// guard rail for the file tools, not a sandbox.
func ResolvePath(ctx context.Context, p string) (string, error) {
	dir := WorkDir(ctx)
	if dir == "" {
		return p, nil
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(dir, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside your worktree %s — use paths relative to it", p, dir)
	}
	return p, nil
}

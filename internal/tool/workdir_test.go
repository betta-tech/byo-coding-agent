package tool

import (
	"context"
	"testing"
)

func TestResolvePathWithoutWorkDirIsIdentity(t *testing.T) {
	got, err := ResolvePath(context.Background(), "some/file.go")
	if err != nil || got != "some/file.go" {
		t.Fatalf("got (%q, %v), want (some/file.go, nil)", got, err)
	}
}

func TestResolvePathAnchorsRelativePaths(t *testing.T) {
	ctx := WithWorkDir(context.Background(), "/repo/.worktrees/a")
	got, err := ResolvePath(ctx, "internal/x.go")
	if err != nil || got != "/repo/.worktrees/a/internal/x.go" {
		t.Fatalf("got (%q, %v)", got, err)
	}
}

func TestResolvePathRejectsEscapes(t *testing.T) {
	ctx := WithWorkDir(context.Background(), "/repo/.worktrees/a")
	for _, p := range []string{"../b/x.go", "/repo/main.go", "/etc/passwd", ".."} {
		if _, err := ResolvePath(ctx, p); err == nil {
			t.Errorf("ResolvePath(%q) should have been rejected", p)
		}
	}
}

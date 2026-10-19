package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo makes a throwaway repo with one commit so worktrees have a base.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir) // macOS /var → /private/var, matches git's view
	run := func(args ...string) {
		t.Helper()
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(Dir+"/\n"), 0644)
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestCreateDiffStatMergeRemove(t *testing.T) {
	repo := newRepo(t)
	w, err := Create(repo, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !Exists(repo, "alpha") {
		t.Fatal("Exists should report the new worktree")
	}

	os.WriteFile(filepath.Join(w.Path, "a.txt"), []byte("one\nTWO\n"), 0644)
	os.WriteFile(filepath.Join(w.Path, "new.txt"), []byte("x\ny\nz\n"), 0644)
	added, removed, err := w.DiffStat()
	if err != nil {
		t.Fatal(err)
	}
	if added != 4 || removed != 1 {
		t.Fatalf("DiffStat = +%d -%d, want +4 -1", added, removed)
	}

	// The main checkout must not see the agent's edits until merge.
	if data, _ := os.ReadFile(filepath.Join(repo, "a.txt")); strings.Contains(string(data), "TWO") {
		t.Fatal("worktree edit leaked into the main checkout")
	}
	if _, err := w.Merge("agent alpha"); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "new.txt")); string(data) != "x\ny\nz\n" {
		t.Fatalf("merge didn't bring new.txt over: %q", data)
	}

	if err := w.Remove(); err != nil {
		t.Fatal(err)
	}
	if Exists(repo, "alpha") {
		t.Fatal("worktree/branch still present after Remove")
	}
}

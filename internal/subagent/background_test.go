package subagent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/betta-tech/byo-coding-agent/internal/api"
	"github.com/betta-tech/byo-coding-agent/internal/provider"
	"github.com/betta-tech/byo-coding-agent/internal/tool"
)

// tempRepo creates a git repo with one commit and makes it the cwd, since
// Spawn finds the repo from the cwd like the real harness does.
func tempRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".worktrees/\n"), 0644)
	if out, err := exec.Command("sh", "-c", "cd "+dir+" && git add -A && git commit -q -m init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	t.Chdir(dir)
	return dir
}

func waitSettled(t *testing.T, f *Fleet, name string) Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range f.Snapshot() {
			if s.Name == name && s.State.Settled() {
				return s
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %s never settled", name)
	return Status{}
}

func TestFleetSpawnRunsInWorktreeAndMerges(t *testing.T) {
	repo := tempRepo(t)
	p := provider.NewMockProvider(
		api.Response{StopReason: api.StopToolUse, Content: []api.Block{{
			Type: api.BlockToolUse, ToolUseID: "t1", ToolName: "write_file",
			ToolInput: `{"path":"feature.txt","content":"a\nb\n"}`,
		}}},
		api.Response{StopReason: api.StopEndTurn, Content: []api.Block{{Type: api.BlockText, Text: "added feature.txt"}}},
	)
	f := &Fleet{Provider: p, Tools: tool.Default.Subset("write_file", "read_file")}

	st, err := f.Spawn("My Feature!", "add a feature file")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "my-feature" {
		t.Fatalf("name not sanitized: %q", st.Name)
	}

	done := waitSettled(t, f, st.Name)
	if done.State != StateDone || done.Added != 2 || done.ToolCalls != 1 {
		t.Fatalf("unexpected final status: %+v", done)
	}
	// The relative path landed in the worktree, not the main checkout.
	if _, err := os.Stat(filepath.Join(repo, "feature.txt")); err == nil {
		t.Fatal("write escaped the worktree into the main checkout")
	}
	if !strings.Contains(f.Log(st.Name), "[tool] write_file") {
		t.Fatalf("log missing tool call: %q", f.Log(st.Name))
	}

	notes := f.DrainNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "added feature.txt") {
		t.Fatalf("notes = %q", notes)
	}
	if len(f.DrainNotes()) != 0 {
		t.Fatal("DrainNotes should hand out each note once")
	}

	if _, err := f.Merge(st.Name); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(repo, "feature.txt")); string(data) != "a\nb\n" {
		t.Fatalf("merge didn't land feature.txt: %q", data)
	}
	if s := f.Snapshot()[0]; s.State != StateMerged {
		t.Fatalf("state after merge = %s", s.State)
	}
}

func TestFleetDiscardAndUniqueNames(t *testing.T) {
	tempRepo(t)
	f := &Fleet{Provider: provider.NewMockProvider(), Tools: tool.NewRegistry()}

	a, err := f.Spawn("same", "x")
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.Spawn("same", "y")
	if err != nil {
		t.Fatal(err)
	}
	if a.Name == b.Name {
		t.Fatalf("both agents got name %q", a.Name)
	}
	waitSettled(t, f, a.Name)
	if err := f.Discard(a.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.Path); err == nil {
		t.Fatal("worktree still on disk after discard")
	}
	if _, err := f.Merge(a.Name); err == nil {
		t.Fatal("merging a discarded agent should fail")
	}
}

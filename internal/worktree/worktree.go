// Package worktree wraps the handful of git commands background agents
// need: one isolated checkout per agent, a live diffstat for the sidebar,
// and merge/remove once the user has decided what to keep.
//
// A worktree is a second working directory backed by the same .git, on
// its own branch. Agents editing the same file in different worktrees
// never see each other's changes until the user merges a branch.
package worktree

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Dir is where worktrees live, relative to the repo root. Gitignored.
const Dir = ".worktrees"

type Worktree struct {
	Repo   string // main checkout root
	Path   string // absolute path of this worktree
	Branch string // agent/<name>
	Base   string // commit the branch started from; diffs are against it
}

// RepoRoot returns the top level of the git repo containing the cwd.
func RepoRoot() (string, error) {
	out, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("background agents need a git repository: %w", err)
	}
	return out, nil
}

// Create adds a worktree for name on a fresh branch off HEAD. Uncommitted
// changes in the main checkout are NOT carried over — agents start from
// the last commit, which is worth telling users about.
func Create(repo, name string) (*Worktree, error) {
	base, err := git(repo, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	w := &Worktree{
		Repo:   repo,
		Path:   filepath.Join(repo, Dir, name),
		Branch: "agent/" + name,
		Base:   base,
	}
	if _, err := git(repo, "worktree", "add", "-b", w.Branch, w.Path, base); err != nil {
		return nil, err
	}
	return w, nil
}

// Exists reports whether a worktree directory or branch already uses name,
// so callers can pick a unique one before calling Create.
func Exists(repo, name string) bool {
	if _, err := os.Stat(filepath.Join(repo, Dir, name)); err == nil {
		return true
	}
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/agent/"+name)
	return err == nil
}

// DiffStat counts lines added/removed relative to Base, including files
// the agent created but git doesn't track yet. Read-only on purpose: it
// runs while the agent works, and staging would race the agent's own git
// commands for index.lock.
func (w *Worktree) DiffStat() (added, removed int, err error) {
	out, err := git(w.Path, "diff", "--numstat", w.Base)
	if err != nil {
		return 0, 0, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" for binary files → 0
		r, _ := strconv.Atoi(f[1])
		added += a
		removed += r
	}
	untracked, err := git(w.Path, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return added, removed, err
	}
	for _, f := range strings.Split(untracked, "\n") {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(w.Path, f))
		if err != nil || len(data) > 1<<20 {
			continue
		}
		added += bytes.Count(data, []byte("\n"))
	}
	return added, removed, nil
}

// Commit snapshots everything in the worktree onto its branch. No-op when
// there's nothing to commit.
func (w *Worktree) Commit(msg string) error {
	if _, err := git(w.Path, "add", "-A"); err != nil {
		return err
	}
	if _, err := git(w.Path, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	_, err := git(w.Path, "commit", "-m", msg)
	return err
}

// Merge commits pending work and merges the branch into whatever the main
// checkout has checked out. --no-ff keeps "this came from agent X" visible
// in history.
func (w *Worktree) Merge(msg string) (string, error) {
	if err := w.Commit(msg); err != nil {
		return "", err
	}
	return git(w.Repo, "merge", "--no-ff", "--no-edit", w.Branch)
}

// Remove deletes the worktree directory and its branch.
func (w *Worktree) Remove() error {
	if _, err := git(w.Repo, "worktree", "remove", "--force", w.Path); err != nil {
		return err
	}
	_, err := git(w.Repo, "branch", "-D", w.Branch)
	return err
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

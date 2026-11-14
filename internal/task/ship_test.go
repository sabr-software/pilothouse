package task

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
)

// withOrigin gives repo a bare origin with main pushed.
func withOrigin(t *testing.T, repo string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin.git")
	mustGit(t, repo, "init", "-q", "--bare", origin)
	mustGit(t, repo, "remote", "add", "origin", origin)
	mustGit(t, repo, "push", "-q", "-u", "origin", "main")
	return origin
}

// pathWith puts a directory holding git (and, when gh is non-empty, a fake
// gh with that script) in front of a PATH without any real gh.
func pathWith(t *testing.T, gh string) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"git", "true", "sh"} {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(p, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	if gh != "" {
		writeFile(t, filepath.Join(bin, "gh"), gh, 0o755)
	}
	// PATH is only the temp dir: a runner with gh in /usr/bin or /bin must
	// still look like a machine without it.
	t.Setenv("PATH", bin)
	if gh == "" {
		if p, err := exec.LookPath("gh"); err == nil {
			t.Fatalf("test PATH still finds gh at %s", p)
		}
	}
}

func newShipTask(t *testing.T, repo string) (*Manager, api.Task) {
	t.Helper()
	m := &Manager{Start: (&recStarter{}).start, Out: io.Discard, Err: io.Discard, Daemon: nopDaemon{}}
	tk, err := m.New(context.Background(), NewOptions{Prompt: "Add a greeting\n\nand more detail", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	return m, tk
}

func TestShipCommitsPushesAndOpensPR(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	origin := withOrigin(t, repo)
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	pathWith(t, "#!/bin/sh\necho \"$PWD|$*|$GH_PROMPT_DISABLED\" > "+ghLog+"\necho https://github.com/x/y/pull/7\n")
	m, tk := newShipTask(t, repo)
	// The agent committed one change and left another uncommitted, plus a new file.
	writeFile(t, filepath.Join(tk.Worktree, "README.md"), "hello\nagent\n", 0o644)
	mustGit(t, tk.Worktree, "commit", "-q", "-am", "agent: readme")
	writeFile(t, filepath.Join(tk.Worktree, "README.md"), "hello\nagent\nleftover\n", 0o644)
	writeFile(t, filepath.Join(tk.Worktree, "new.txt"), "new\n", 0o644)

	done, err := m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || done.PRURL != "https://github.com/x/y/pull/7" || !strings.Contains(done.Note, "opened https://github.com/x/y/pull/7") {
		t.Fatalf("task %+v", done)
	}
	if dirExists(tk.Worktree) {
		t.Error("worktree kept")
	}
	files := mustGit(t, origin, "log", "--name-only", "--format=", "refs/heads/"+tk.Branch)
	if !strings.Contains(files, "new.txt") || strings.Contains(files, ".env") {
		t.Fatalf("pushed files:\n%s", files)
	}
	if subj := mustGit(t, origin, "log", "-1", "--format=%s%n%b", "refs/heads/"+tk.Branch); !strings.HasPrefix(subj, "Add a greeting\n") || !strings.Contains(subj, "Pilothouse-Task: "+tk.ID) {
		t.Fatalf("commit message %q", subj)
	}
	logb, _ := os.ReadFile(ghLog)
	if want := repo + "|pr create --fill --head " + tk.Branch + " --base main|1"; strings.TrimSpace(string(logb)) != want {
		t.Fatalf("gh %q, want %q", logb, want)
	}
	if stored, _ := m.Get(tk.ID); stored.Status != api.TaskDone || stored.PRURL == "" {
		t.Fatalf("stored %+v", stored)
	}
	if _, err := m.Ship(context.Background(), tk.ID); err == nil || !strings.Contains(err.Error(), "already done") {
		t.Fatalf("second ship: %v", err)
	}
}

func TestShipWithoutGhOrOrigin(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	pathWith(t, "")
	m, tk := newShipTask(t, repo)
	writeFile(t, filepath.Join(tk.Worktree, "a.txt"), "a\n", 0o644)
	// No origin: committed on the branch, the person is told how to push.
	done, err := m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || !strings.Contains(done.Note, "no origin remote") || !strings.Contains(done.Note, tk.Branch) {
		t.Fatalf("task %+v", done)
	}
	if files := mustGit(t, repo, "log", "--name-only", "--format=", tk.Branch); !strings.Contains(files, "a.txt") {
		t.Fatalf("not committed: %s", files)
	}

	// Origin but no gh: pushed, open the PR yourself.
	withOrigin(t, repo)
	m, tk = newShipTask(t, repo)
	writeFile(t, filepath.Join(tk.Worktree, "b.txt"), "b\n", 0o644)
	done, err = m.Ship(context.Background(), tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || done.PRURL != "" || !strings.Contains(done.Note, "gh (GitHub CLI) is not installed") {
		t.Fatalf("task %+v", done)
	}
	mustGit(t, repo, "ls-remote", "--exit-code", "origin", "refs/heads/"+tk.Branch)
}

func TestShipRefusals(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	pathWith(t, "")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	ctx := context.Background()

	m, tk := newShipTask(t, repo)
	if _, err := m.Ship(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "nothing to ship") {
		t.Fatalf("empty: %v", err)
	}
	if !dirExists(tk.Worktree) {
		t.Fatal("refusal removed the worktree")
	}
	// The agent committed the copied .env: never pushed anywhere.
	mustGit(t, tk.Worktree, "add", "-f", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "oops")
	if _, err := m.Ship(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "committed .env") {
		t.Fatalf("copied file committed: %v", err)
	}
	if st, _ := m.Get(tk.ID); st.Status != api.TaskRunning {
		t.Fatalf("status after refusal %s", st.Status)
	}

	// A push that fails leaves the task open, and the message carries no
	// credentials from the remote URL.
	m, tk = newShipTask(t, repo)
	mustGit(t, repo, "remote", "add", "origin", "https://alice:s3cr3t-token@127.0.0.1:1/x/y.git")
	writeFile(t, filepath.Join(tk.Worktree, "c.txt"), "c\n", 0o644)
	got, err := m.Ship(ctx, tk.ID)
	if err == nil || !strings.Contains(err.Error(), "git push failed") || strings.Contains(err.Error(), "s3cr3t") || strings.Contains(got.Note, "s3cr3t") {
		t.Fatalf("push failure: %v / %q", err, got.Note)
	}
	if got.Status == api.TaskDone || !dirExists(tk.Worktree) {
		t.Fatalf("task after failed push %+v", got)
	}
	if files := mustGit(t, repo, "log", "--name-only", "--format=", tk.Branch); !strings.Contains(files, "c.txt") {
		t.Fatalf("leftovers not committed before the push: %s", files)
	}

	// A failed task has nothing to ship.
	failed := tk
	failed.ID, failed.Status = "t_failedtask", api.TaskFailed
	if err := saveTask(failed); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ship(ctx, failed.ID); err == nil || !strings.Contains(err.Error(), "never started") {
		t.Fatalf("failed task: %v", err)
	}
}

func TestCloseNothingToShip(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	ctx := context.Background()
	m, tk := newShipTask(t, repo)
	tk.Status = api.TaskReview
	if err := saveTask(tk); err != nil {
		t.Fatal(err)
	}

	// Committed work means there is something to ship, not close.
	writeFile(t, filepath.Join(tk.Worktree, "a.txt"), "a\n", 0o644)
	mustGit(t, tk.Worktree, "add", "a.txt")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "work")
	if _, err := m.Close(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "ship or discard") {
		t.Fatalf("close with a commit: %v", err)
	}
	if !dirExists(tk.Worktree) {
		t.Fatal("refusal removed the worktree")
	}
	mustGit(t, tk.Worktree, "reset", "-q", "--hard", tk.BaseRef)

	// Uncommitted work is equally not a no-op.
	writeFile(t, filepath.Join(tk.Worktree, "b.txt"), "b\n", 0o644)
	if _, err := m.Close(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("close with leftovers: %v", err)
	}
	if !dirExists(tk.Worktree) {
		t.Fatal("refusal removed the worktree")
	}
	if err := os.Remove(filepath.Join(tk.Worktree, "b.txt")); err != nil {
		t.Fatal(err)
	}

	done, err := m.Close(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != api.TaskDone || done.PRURL != "" || !strings.Contains(done.Note, "No changes to ship") || !strings.Contains(done.Note, tk.Branch) {
		t.Fatalf("closed %+v", done)
	}
	if dirExists(tk.Worktree) {
		t.Error("worktree kept")
	}
	mustGit(t, repo, "rev-parse", "--verify", tk.Branch)
	if _, err := m.Close(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), "not ready for review") {
		t.Fatalf("second close: %v", err)
	}

	// Only review tasks can be closed.
	m2, tk2 := newShipTask(t, repo)
	if _, err := m2.Close(ctx, tk2.ID); err == nil || !strings.Contains(err.Error(), "not ready for review") {
		t.Fatalf("close while running: %v", err)
	}
}

// An agent that detaches HEAD (git checkout --detach, a rebase stopped
// half-way) or switches branches commits where the task's branch does not
// see it. Removing the worktree would drop those commits, so nothing that
// removes it goes ahead.
func TestFinishRefusesWorkOffTheBranch(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	pathWith(t, "")
	ctx := context.Background()
	detach := []string{"checkout", "-q", "--detach"}

	for _, tc := range []struct {
		name   string
		leave  []string // how the agent left the branch
		where  string
		finish func(m *Manager, id string) (api.Task, error)
	}{
		{"close detached", detach, "a detached HEAD", func(m *Manager, id string) (api.Task, error) { return m.Close(ctx, id) }},
		{"ship detached", detach, "a detached HEAD", func(m *Manager, id string) (api.Task, error) { return m.Ship(ctx, id) }},
		{"done detached", detach, "a detached HEAD", func(m *Manager, id string) (api.Task, error) { return m.Done(ctx, id, DoneOptions{}) }},
		{"done --force detached", detach, "a detached HEAD", func(m *Manager, id string) (api.Task, error) {
			return m.Done(ctx, id, DoneOptions{Force: true})
		}},
		{"ship other branch", []string{"checkout", "-q", "-b", "side"}, "branch side", func(m *Manager, id string) (api.Task, error) { return m.Ship(ctx, id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, tk := newShipTask(t, repo)
			tk.Status = api.TaskReview
			if err := saveTask(tk); err != nil {
				t.Fatal(err)
			}
			mustGit(t, tk.Worktree, tc.leave...)
			writeFile(t, filepath.Join(tk.Worktree, "work.txt"), "the agent's work\n", 0o644)
			mustGit(t, tk.Worktree, "add", "work.txt")
			mustGit(t, tk.Worktree, "commit", "-q", "-m", "work off the branch")
			sha := mustGit(t, tk.Worktree, "rev-parse", "HEAD")
			if strings.HasPrefix(tc.name, "ship") {
				// Uncommitted work too: Ship would commit it where HEAD is.
				writeFile(t, filepath.Join(tk.Worktree, "more.txt"), "uncommitted\n", 0o644)
			}

			got, err := tc.finish(m, tk.ID)
			if err == nil {
				t.Fatalf("finished a task whose work is on %s: %+v", tc.where, got)
			}
			for _, want := range []string{tc.where, tk.Branch, tk.Worktree, "git merge " + sha[:7]} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q: %v", want, err)
				}
			}
			if !dirExists(tk.Worktree) {
				t.Fatal("refusal removed the worktree")
			}
			if head := mustGit(t, tk.Worktree, "rev-parse", "HEAD"); head != sha {
				t.Fatalf("worktree HEAD moved to %s, was %s", head, sha)
			}
			if st, _ := m.Get(tk.ID); st.Status != api.TaskReview {
				t.Fatalf("status after refusal %s", st.Status)
			}

			// What the message says to do makes the task shippable, and
			// the work ends up on the branch.
			mustGit(t, tk.Worktree, "checkout", "-q", tk.Branch)
			mustGit(t, tk.Worktree, "merge", "-q", sha)
			done, err := m.Ship(ctx, tk.ID)
			if err != nil {
				t.Fatal(err)
			}
			if done.Status != api.TaskDone || dirExists(tk.Worktree) {
				t.Fatalf("after the fix-up: %+v", done)
			}
			if files := mustGit(t, repo, "log", "--name-only", "--format=", tk.Branch); !strings.Contains(files, "work.txt") {
				t.Fatalf("work not on %s: %s", tk.Branch, files)
			}
		})
	}

	// Keeping the worktree loses nothing, so `done --keep` still works.
	m, tk := newShipTask(t, repo)
	mustGit(t, tk.Worktree, detach...)
	if done, err := m.Done(ctx, tk.ID, DoneOptions{Keep: true}); err != nil || done.Status != api.TaskDone || !dirExists(tk.Worktree) {
		t.Fatalf("done --keep on a detached HEAD: %v %+v", err, done)
	}
}

// A copied file that was committed and then removed in a later commit is
// gone from the branch's tip but still in what a push sends.
func TestShipRefusesCopiedFileInHistory(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	origin := withOrigin(t, repo)
	pathWith(t, "")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	ctx := context.Background()

	m, tk := newShipTask(t, repo)
	mustGit(t, tk.Worktree, "add", "-f", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "oops")
	oops := mustGit(t, tk.Worktree, "rev-parse", "--short", "HEAD")
	mustGit(t, tk.Worktree, "rm", "-q", "--cached", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "remove the secret again")
	writeFile(t, filepath.Join(tk.Worktree, "a.txt"), "a\n", 0o644)

	got, err := m.Ship(ctx, tk.ID)
	if err == nil {
		t.Fatalf("shipped a branch with .env in its history: %+v", got)
	}
	for _, want := range []string{"not shipping", ".env", oops, "history"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if out := mustGit(t, origin, "for-each-ref", "refs/heads/"+tk.Branch); out != "" {
		t.Fatalf("the branch was pushed: %s", out)
	}
	if st, _ := m.Get(tk.ID); st.Status != api.TaskRunning || !dirExists(tk.Worktree) {
		t.Fatalf("task after refusal %+v", st)
	}

	// With the history rewritten, the same work ships.
	mustGit(t, tk.Worktree, "reset", "-q", "--soft", tk.BaseRef)
	mustGit(t, tk.Worktree, "reset", "-q")
	if done, err := m.Ship(ctx, tk.ID); err != nil || done.Status != api.TaskDone {
		t.Fatalf("after rewriting history: %v %+v", err, done)
	}
	if files := mustGit(t, origin, "log", "--name-only", "--format=", "refs/heads/"+tk.Branch); !strings.Contains(files, "a.txt") || strings.Contains(files, ".env") {
		t.Fatalf("pushed files:\n%s", files)
	}

	// The same on a side branch the agent merged back in: a push sends
	// those commits too.
	m, tk = newShipTask(t, repo)
	mustGit(t, tk.Worktree, "checkout", "-q", "-b", "scratch")
	mustGit(t, tk.Worktree, "add", "-f", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "oops")
	mustGit(t, tk.Worktree, "rm", "-q", "--cached", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "remove the secret again")
	mustGit(t, tk.Worktree, "checkout", "-q", tk.Branch)
	writeFile(t, filepath.Join(tk.Worktree, "b.txt"), "b\n", 0o644)
	mustGit(t, tk.Worktree, "add", "b.txt")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "work")
	mustGit(t, tk.Worktree, "merge", "-q", "--no-ff", "-m", "merge scratch", "scratch")
	if got, err := m.Ship(ctx, tk.ID); err == nil || !strings.Contains(err.Error(), ".env in its history") {
		t.Fatalf("merged side branch with .env: %v %+v", err, got)
	}
	if out := mustGit(t, origin, "for-each-ref", "refs/heads/"+tk.Branch); out != "" {
		t.Fatalf("the branch was pushed: %s", out)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"fatal: unable to access 'https://bob:pw@github.com/x/y.git/'": "fatal: unable to access 'https://***@github.com/x/y.git/'",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123 rejected":            "token *** rejected",
		"ssh://git@github.com/x/y":                                     "ssh://***@github.com/x/y",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

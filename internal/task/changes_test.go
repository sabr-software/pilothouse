package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
)

// agentWork makes the kind of mess a real agent leaves in a worktree:
// commits, uncommitted edits, a rename, a deletion, a binary change, new
// files, and an untracked symlink pointing at a secret outside the repo.
func agentWork(t *testing.T, wt, secret string) {
	t.Helper()
	writeFile(t, filepath.Join(wt, "README.md"), "hello\ncommitted line\n", 0o644)
	mustGit(t, wt, "add", "README.md")
	mustGit(t, wt, "commit", "-q", "-m", "agent: readme")
	mustGit(t, wt, "mv", "old.txt", "renamed.txt")
	mustGit(t, wt, "rm", "-q", "gone.txt")
	writeFile(t, filepath.Join(wt, "main.go"), "package main\n\nfunc main() {}\n// uncommitted\n", 0o644)
	writeFile(t, filepath.Join(wt, "logo.bin"), "\x00\x01\x02new", 0o644)
	writeFile(t, filepath.Join(wt, "notes.txt"), "first\nsecond", 0o644)
	if err := os.Symlink(secret, filepath.Join(wt, "leak")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(wt, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoWithFiles(t *testing.T) string {
	t.Helper()
	repo := newRepo(t)
	writeFile(t, filepath.Join(repo, "old.txt"), strings.Repeat("same content\n", 20), 0o644)
	writeFile(t, filepath.Join(repo, "gone.txt"), "bye\n", 0o644)
	writeFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc main() {}\n", 0o644)
	writeFile(t, filepath.Join(repo, "logo.bin"), "\x00\x01\x02old", 0o644)
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "files")
	return repo
}

func TestChanges(t *testing.T) {
	testEnv(t)
	repo := repoWithFiles(t)
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=hunter2\n", 0o600)
	writeTaskYAML(t, repo, "copy: [.env]\n")
	m, _ := newManager(t)
	m.Start = (&recStarter{}).start
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "work", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "id_rsa")
	writeFile(t, secret, "PRIVATE KEY MATERIAL", 0o600)
	agentWork(t, tk.Worktree, secret)

	d, err := Changes(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]api.FilePreview{}
	var paths []string
	for _, f := range d.Files {
		byPath[f.Path] = f
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "README.md,gone.txt,leak,logo.bin,main.go,notes.txt,renamed.txt" {
		t.Fatalf("files %s", got)
	}
	check := func(p string, op api.FileOp, added, removed int, diffHas ...string) {
		t.Helper()
		f := byPath[p]
		if f.Op != op || f.Added != added || f.Removed != removed {
			t.Errorf("%s: %+v, want op %s +%d -%d", p, f, op, added, removed)
		}
		for _, s := range diffHas {
			if !strings.Contains(f.Diff, s) {
				t.Errorf("%s diff lacks %q:\n%s", p, s, f.Diff)
			}
		}
		if strings.HasPrefix(f.Diff, "diff --git") || strings.Contains(f.Diff, "\n+++ ") {
			t.Errorf("%s diff keeps its header:\n%s", p, f.Diff)
		}
	}
	check("README.md", api.FileEdit, 1, 0, "@@", "+committed line")
	check("main.go", api.FileEdit, 1, 0, "+// uncommitted")
	check("gone.txt", api.FileDelete, 0, 1, "-bye")
	check("renamed.txt", api.FileRename, 0, 0)
	check("logo.bin", api.FileEdit, 0, 0)
	check("notes.txt", api.FileCreate, 2, 0, "@@ -0,0 +1,2 @@\n+first\n+second\n\\ No newline at end of file")
	// The untracked symlink shows where it points, never what it points at.
	check("leak", api.FileCreate, 1, 0, "+"+secret)
	for _, f := range d.Files {
		if strings.Contains(f.Diff, "PRIVATE KEY") || strings.Contains(f.Diff, "hunter2") {
			t.Fatalf("%s leaks a secret:\n%s", f.Path, f.Diff)
		}
	}
	if byPath["main.go"].Lang != "go" || byPath["README.md"].Lang != "md" {
		t.Errorf("lang %q %q", byPath["main.go"].Lang, byPath["README.md"].Lang)
	}
	if d.Added != 5 || d.Removed != 1 || d.Truncated || d.TaskID != tk.ID {
		t.Errorf("totals %+v", d)
	}
	// The agent's index and working tree are left exactly as they were.
	if st := mustGit(t, tk.Worktree, "status", "--porcelain"); !strings.Contains(st, "?? notes.txt") || !strings.Contains(st, "R  old.txt -> renamed.txt") {
		t.Errorf("status changed: %s", st)
	}

	// Without a worktree: the branch's commits only.
	if err := os.Remove(filepath.Join(tk.Worktree, "pipe")); err != nil {
		t.Fatal(err)
	}
	mustGit(t, tk.Worktree, "add", "-A")
	mustGit(t, tk.Worktree, "reset", "-q", "--", ".env")
	mustGit(t, tk.Worktree, "commit", "-q", "-m", "rest")
	mustGit(t, repo, "worktree", "remove", "--force", tk.Worktree)
	d, err = Changes(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	paths = nil
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "README.md,gone.txt,leak,logo.bin,main.go,notes.txt,renamed.txt" {
		t.Fatalf("committed files %s", got)
	}
	mustGit(t, repo, "branch", "-D", tk.Branch)
	if _, err := Changes(ctx, tk); err == nil || !strings.Contains(err.Error(), "are gone") {
		t.Fatalf("no branch: %v", err)
	}
}

func TestChangesBounded(t *testing.T) {
	testEnv(t)
	repo := newRepo(t)
	m, _ := newManager(t)
	m.Start = (&recStarter{}).start
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "big", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	writeFile(t, filepath.Join(tk.Worktree, "README.md"), strings.Repeat(line, 2000), 0o644) // ~200 KB tracked edit
	writeFile(t, filepath.Join(tk.Worktree, "huge.log"), strings.Repeat(line, 15000), 0o644) // 1.5 MB untracked
	for i := 0; i < maxDiffFiles+10; i++ {
		writeFile(t, filepath.Join(tk.Worktree, "many", fmt.Sprintf("f%03d", i)), "x\n", 0o644)
	}
	d, err := Changes(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len(d.Files) != maxDiffFiles {
		t.Fatalf("truncated=%v files=%d", d.Truncated, len(d.Files))
	}
	total := 0
	for _, f := range d.Files {
		total += len(f.Diff)
		if len(f.Diff) > maxFileDiff {
			t.Errorf("%s: %d bytes of diff", f.Path, len(f.Diff))
		}
		switch f.Path {
		case "README.md":
			if !f.Truncated || f.Added != 2000 || f.Removed != 1 {
				t.Errorf("README.md %+v", f.Added)
			}
		case "huge.log":
			if !f.Truncated {
				t.Errorf("huge.log not truncated")
			}
		}
	}
	if total > maxTotalDiff+maxFileDiff {
		t.Errorf("total diff %d bytes", total)
	}
}

// A file that becomes a symlink has two sections in git's patch (the file
// deleted, the link created) for its one entry in --raw. Every later file
// must still get its own diff, however its name is written in the patch
// header and whatever prefixes the user's git config asks for.
func TestChangesMatchesDiffsByPath(t *testing.T) {
	testEnv(t)
	repo := repoWithFiles(t)
	names := []string{"b.txt", "my file.txt", "ta\tb.txt", `we"ird.txt`, `back\slash.txt`, "ünï.txt"}
	writeFile(t, filepath.Join(repo, "a.txt"), "a regular file\n", 0o644)
	for _, n := range names {
		writeFile(t, filepath.Join(repo, n), "base\n", 0o644)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "more files")
	m, _ := newManager(t)
	m.Start = (&recStarter{}).start
	ctx := context.Background()
	tk, err := m.New(ctx, NewOptions{Prompt: "links", Agent: "true", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	wt := tk.Worktree
	if err := os.Remove(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/somewhere/else", filepath.Join(wt, "a.txt")); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		writeFile(t, filepath.Join(wt, n), "base\nchange to "+n+"\n", 0o644)
	}
	mustGit(t, wt, "mv", "old.txt", "renamed.txt")
	writeFile(t, filepath.Join(wt, "renamed.txt"), strings.Repeat("same content\n", 20)+"renamed and edited\n", 0o644)

	check := func(t *testing.T) {
		t.Helper()
		d, err := Changes(ctx, tk)
		if err != nil {
			t.Fatal(err)
		}
		byPath := map[string]api.FilePreview{}
		for _, f := range d.Files {
			byPath[f.Path] = f
		}
		for _, n := range names {
			f, ok := byPath[n]
			if want := "@@ -1 +1,2 @@\n base\n+change to " + n + "\n"; !ok || f.Diff != want || f.Added != 1 || f.Removed != 0 {
				t.Errorf("%q: +%d -%d, diff %q, want %q", n, f.Added, f.Removed, f.Diff, want)
			}
		}
		if f := byPath["renamed.txt"]; f.Op != api.FileRename || !strings.Contains(f.Diff, "+renamed and edited") || strings.Contains(f.Diff, "change to") {
			t.Errorf("renamed.txt: %+v", f)
		}
		// The type change shows both halves: the file going, the link coming.
		a := byPath["a.txt"]
		if !strings.Contains(a.Diff, "-a regular file") || !strings.Contains(a.Diff, "+/somewhere/else") || strings.Contains(a.Diff, "diff --git") || strings.Contains(a.Diff, "change to") {
			t.Errorf("a.txt: %q", a.Diff)
		}
		if d.Truncated {
			t.Errorf("truncated: %+v", d)
		}
	}
	check(t)

	// A user's diff.noprefix or diff.mnemonicPrefix changes git's headers.
	for _, cfg := range []string{"diff.noprefix", "diff.mnemonicPrefix"} {
		t.Run(cfg, func(t *testing.T) {
			mustGit(t, repo, "config", cfg, "true")
			defer mustGit(t, repo, "config", "--unset", cfg)
			check(t)
		})
	}

	// The same for a branch whose worktree is gone.
	mustGit(t, wt, "add", "-A")
	mustGit(t, wt, "commit", "-q", "-m", "all of it")
	mustGit(t, repo, "worktree", "remove", "--force", wt)
	check(t)
}

func TestGitQuote(t *testing.T) {
	for in, want := range map[string]string{
		"a/plain.txt":       "a/plain.txt",
		"a/my file.txt":     "a/my file.txt",
		"a/ünï.txt":         "a/ünï.txt",
		"a/ta\tb.txt":       `"a/ta\tb.txt"`,
		`a/we"ird.txt`:      `"a/we\"ird.txt"`,
		`a/back\slash.txt`:  `"a/back\\slash.txt"`,
		"a/new\nline.txt":   `"a/new\nline.txt"`,
		"a/esc\x1bdel\x7f1": `"a/esc\033del\1771"`,
	} {
		if got := gitQuote(in); got != want {
			t.Errorf("gitQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

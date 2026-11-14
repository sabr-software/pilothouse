package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
)

// A person's `path:` rule names a file. Another spelling of the same file
// (different letter case on a file system that ignores case, or a path
// through a symlink) is the same file, and a deny rule must hold for it.
func TestPathRulesFollowTheFileNotTheSpelling(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: no-secrets, match: {path: "secrets/**"}, action: deny}
  - {name: no-env, match: {path: ".env*"}, action: deny}
  - {name: no-keys, match: {path: "~/Keys/**"}, action: deny}
  - {name: allow-lib, match: {path: "lib/**"}, action: allow}
`)
	writeFile(t, filepath.Join(te.repo, "secrets", "prod.key"), "k")
	writeFile(t, filepath.Join(te.repo, "lib", "a.go"), "x")
	writeFile(t, filepath.Join(te.home, "Keys", "id"), "k")
	if err := os.Symlink(filepath.Join(te.repo, "secrets"), filepath.Join(te.repo, "lib", "vault")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(te.home, "Keys"), filepath.Join(te.repo, "k")); err != nil {
		t.Fatal(err)
	}
	e, _ := te.load(t)
	edit := func(p string) Input {
		return Input{Agent: api.AgentClaude, Tool: "Write", ToolKind: api.ToolEdit, Paths: []string{p}, Cwd: te.repo, Repo: te.repo}
	}

	tests := []struct {
		name, path string
		rule       string // "" for no rule decided
		macOnly    bool   // only on a file system that ignores case
	}{
		{"as written", "secrets/prod.key", "no-secrets", false},
		{"other case", "SECRETS/prod.key", "no-secrets", true},
		{"other case, absolute", filepath.Join(te.repo, "Secrets", "new.key"), "no-secrets", true},
		{"dot env, other case", ".ENV.local", "no-env", true},
		{"home rule, other case", "~/keys/id", "no-keys", true},
		// The allow rule sees lib/vault/x; the file is secrets/x.
		{"through a symlinked directory", "lib/vault/prod.key", "no-secrets", false},
		{"new file through a symlinked directory", "lib/vault/new.key", "no-secrets", false},
		{"through a symlink to home", "k/id", "no-keys", false},
		{"new directories through a symlinked directory", "lib/vault/a/b/c/new.key", "no-secrets", false},
		{"ordinary file", "lib/a.go", "allow-lib", false},
		{"ordinary file, other case", "LIB/a.go", "allow-lib", true},
		{"lookalike", "secrets2/prod.key", "", false},
	}
	for _, tt := range tests {
		if tt.macOnly && !caseInsensitiveFS {
			continue
		}
		r := e.Evaluate(edit(tt.path))
		if r.Rule != tt.rule {
			t.Errorf("%s (%s): rule %q (%s), want %q", tt.name, tt.path, r.Rule, r.Action, tt.rule)
		}
	}
}

// An allow rule is about where the file really is, and about every file of
// the call.
func TestAllowPathRulesNeedTheRealFile(t *testing.T) {
	te := newTestEnv(t)
	real := filepath.Join(te.home, "real")
	link := filepath.Join(te.home, "link")
	writeFile(t, filepath.Join(real, "work", "a.txt"), "x")
	writeFile(t, filepath.Join(te.home, "other", "b.txt"), "x")
	writeFile(t, filepath.Join(te.repo, "lib", "a.go"), "x")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(te.home, "other"), filepath.Join(te.repo, "lib", "out")); err != nil {
		t.Fatal(err)
	}
	// The rule is written with the symlinked spelling, as /tmp/** is on
	// macOS; it holds for both spellings of the files under it.
	writeFile(t, te.userFile(), `version: 1
rules:
  - {name: allow-link, match: {path: "~/link/work/**"}, action: allow}
  - {name: allow-lib, match: {path: "lib/**"}, action: allow}
  - {name: ask-other, match: {kind: edit}, when: 'paths.exists(p, p.contains("/other/"))', action: ask}
`)
	e, _ := te.load(t)
	edit := func(paths ...string) Input {
		return Input{Agent: api.AgentClaude, Tool: "Write", ToolKind: api.ToolEdit, Paths: paths, Cwd: te.repo, Repo: te.repo}
	}
	tests := []struct {
		name  string
		paths []string
		rule  string
	}{
		{"rule and call use the symlinked spelling", []string{filepath.Join(link, "work", "a.txt")}, "allow-link"},
		{"call uses the real spelling", []string{filepath.Join(real, "work", "a.txt")}, "allow-link"},
		{"outside the rule", []string{filepath.Join(real, "elsewhere.txt")}, ""},
		{"every file inside", []string{"lib/a.go", "lib/b.go"}, "allow-lib"},
		{"one file outside", []string{"lib/a.go", "docs/readme.md"}, ""},
		// lib/out is a symlink out of lib: not what allow-lib allows, and
		// a condition on the real location sees it.
		{"symlink out of the allowed directory", []string{"lib/out/b.txt"}, "ask-other"},
	}
	for _, tt := range tests {
		r := e.Evaluate(edit(tt.paths...))
		if r.Rule != tt.rule {
			t.Errorf("%s %v: rule %q (%s), want %q", tt.name, tt.paths, r.Rule, r.Action, tt.rule)
		}
	}
}

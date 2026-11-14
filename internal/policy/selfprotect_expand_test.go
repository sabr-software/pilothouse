package policy

import (
	"path/filepath"
	"testing"
)

// The shell rewrites a command before it runs: globs, braces, variables set
// on the same line, and wrappers that change directory. A protected path
// reached that way is still a protected path.
func TestSelfProtectionSeesShellExpansions(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: yolo, match: {tool: \"*\"}, action: allow}\n")
	e, _ := te.load(t)
	wt := filepath.Join(te.agHome, "worktrees", "app", "fix-login")

	tests := []struct {
		cmd  string
		cwd  string
		want bool
	}{
		// Globs that can only name the protected directory or a file in it.
		{"rm ~/.pilot*/policy.yaml", "", true},
		{"echo x > ~/.pilot*/policy.yaml", "", true},
		{"cp /tmp/x ~/.pilothous?/policy.yaml", "", true},
		{"rm ~/.[p]ilothouse/policy.yaml", "", true},
		{"rm -rf ~/.pilothouse*", "", true},
		{"rm -rf ~/.*", "", true},
		{"rm ~/.pilot{house,x}/policy.yaml", "", true},
		{"rm ~/.pilot{house}/policy.yaml", "", false}, // no comma: literal braces
		{"tee ~/.clau*/settings.json < x", "", true},
		{"rm $HOME/.pilot*/config.yaml", "", true},
		{"cd ~/.pilot* && rm policy.yaml", "", true},
		{"rm -rf .pilot*", "", true}, // the repo's own policy directory
		{"rm .pilothouse/pol*", "", true},
		{"rm ~/**/policy.yaml", "", false}, // ** skips dot directories
		{"shopt -s dotglob; rm -rf ~/*", "", true},
		{"shopt -s globstar dotglob; rm ~/**/policy.yaml", "", true},
		// Wrappers that change directory.
		{"env -C ~/.pilothouse rm policy.yaml", "", true},
		{"env --chdir=~/.pilothouse rm policy.yaml", "", true},
		{"env --chdir ~/.pilothouse rm policy.yaml", "", true},
		{"sudo -D ~/.pilothouse rm policy.yaml", "", true},
		{"sudo --chdir=~/.pilothouse rm policy.yaml", "", true},
		// Variables set earlier on the same line.
		{"D=~/.pilothouse; rm $D/policy.yaml", "", true},
		{`DIR="$HOME/.pilothouse" && rm "$DIR/policy.yaml"`, "", true},
		{`export CFG=~/.pilothouse/policy.yaml; echo x > "$CFG"`, "", true},
		{"d=.pilothouse; rm ~/${d}/policy.yaml", "", true},
		{"f=policy.yaml; cd ~/.pilothouse && rm $f", "", true},

		// Ordinary commands keep running.
		{"rm -rf dist/*", "", false},
		{"rm -rf ./*", "", false},
		{"rm *.log", "", false},
		{"rm -rf ~/*.tmp", "", false},
		{"rm -rf ~/*", "", false}, // * skips dot files
		{"cp src/*.ts out/", "", false},
		{"rm app/[id]/page.tsx", "", false},
		{"chmod +x scripts/*.sh", "", false},
		{"rm ~/.pilothousex*", "", false},
		{"rm ~/.pilot*x/policy.yaml", "", false},
		{"ls ~/.pilot*", "", false},
		{"cat ~/.pilot*/policy.yaml", "", false},
		{"env -C /tmp rm policy.yaml", "", false},
		{"env -C ~/.pilothouse ls", "", false},
		{"D=/tmp/build; rm -rf $D/out", "", false},
		{"rm -rf $UNKNOWN/out", "", false},
		{`git commit -m "handle ~/.pilot* globs"`, "", false},
		// A task's worktree lives under Pilothouse's home; globs there are
		// the agent's own files.
		{"rm *.log", wt, false},
		{"rm -rf build/*", wt, false},
		{"rm -rf .pilot*", wt, true}, // the worktree's own repo policy
	}
	for _, tt := range tests {
		in := shell(tt.cmd)
		in.Cwd, in.Repo = te.repo, te.repo
		if tt.cwd != "" {
			in.Cwd, in.Repo = tt.cwd, tt.cwd
		}
		r := e.Evaluate(in)
		if got := r.Rule == SelfProtectionRule; got != tt.want {
			t.Errorf("%q: self-protection=%v, want %v (%s)", tt.cmd, got, tt.want, r.Message)
		}
	}
}

// Project settings are protected by "**/" globs. A shell pattern is checked
// against the files it names on disk, so an ordinary `rm -rf dist/*` in a
// repo is not a question.
func TestSelfProtectionGlobOverProjectSettings(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: yolo, match: {tool: \"*\"}, action: allow}\n")
	e, _ := Load(DefaultOptions(
		filepath.Join(te.home, ".claude", "settings.json"),
		"**/.claude/settings.json", "**/.claude/settings.local.json", "**/opencode.json",
	))
	writeFile(t, filepath.Join(te.repo, ".claude", "settings.json"), "{}")
	writeFile(t, filepath.Join(te.repo, ".claude", "commands", "x.md"), "x")
	writeFile(t, filepath.Join(te.repo, "opencode.json"), "{}")
	writeFile(t, filepath.Join(te.repo, "dist", "app.js"), "x")
	writeFile(t, filepath.Join(te.repo, "dist", "opencode.json.map"), "x")
	writeFile(t, filepath.Join(te.repo, "src", "settings.json"), "{}")

	tests := []struct {
		cmd  string
		want bool
	}{
		{"rm .claude/sett*.json", true},
		{"rm .claude/*", true},
		{"rm .claude/{settings,other}.json", true},
		{"echo {} > .claude/settings.j*", true},
		{"rm open*.json", true},
		{"rm -rf *", true}, // names opencode.json
		{"rm ./*.json", true},
		{"cd .claude && rm *.json", true},
		{"rm -rf dist/*", false},
		{"rm dist/*.json*", false},
		{"rm src/*.json", false},
		{"rm .claude/commands/*", false},
		{"rm *.log", false},
		{"ls *", false},
		{"cat .claude/*.json", false},
	}
	for _, tt := range tests {
		in := shell(tt.cmd)
		in.Cwd, in.Repo = te.repo, te.repo
		r := e.Evaluate(in)
		if got := r.Rule == SelfProtectionRule; got != tt.want {
			t.Errorf("%q: self-protection=%v, want %v (%s)", tt.cmd, got, tt.want, r.Message)
		}
	}
}

func TestBraceExpand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"a", []string{"a"}},
		{"a{b,c}d", []string{"abd", "acd"}},
		{"{a,b}{c,d}", []string{"ac", "ad", "bc", "bd"}},
		{"a{b,{c,d}}", []string{"ab", "ac", "ad"}},
		{"a{b}c", []string{"a{b}c"}},
		{"a{b}{c,d}", []string{"a{b}c", "a{b}d"}},
		{"a{b,c", []string{"a{b,c"}},
		{"a{,b}", []string{"a", "ab"}},
	}
	for _, tt := range tests {
		got := braceExpand(tt.in, 64)
		if len(got) != len(tt.want) {
			t.Errorf("braceExpand(%q) = %q, want %q", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("braceExpand(%q) = %q, want %q", tt.in, got, tt.want)
				break
			}
		}
	}
	if got := braceExpand("{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}", 64); len(got) != 64 {
		t.Errorf("unbounded expansion: %d words", len(got))
	}
}

// A pattern that stands for more names than are checked is a question, not
// a pass: otherwise padding a command would switch the check off.
func TestSelfProtectionAsksAboutPatternsTooWideToCheck(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: yolo, match: {tool: \"*\"}, action: allow}\n")
	e, _ := Load(DefaultOptions(filepath.Join(te.home, ".claude", "settings.json"), "**/opencode.json"))
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		writeFile(t, filepath.Join(te.repo, "pkg", n, "x.js"), "x")
	}
	old := maxPatternNames
	maxPatternNames = 4
	t.Cleanup(func() { maxPatternNames = old })

	for cmd, want := range map[string]bool{
		"rm pkg/*/*.js":  true,  // six directories, four may be read
		"rm pkg/a/*.js":  false, // one entry
		"rm pkg/a/x.js":  false,
		"cat pkg/*/*.js": false, // reading
		"rm ~/{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{a,b}{,x}/f": true, // 512 alternatives
	} {
		in := shell(cmd)
		in.Cwd, in.Repo = te.repo, te.repo
		r := e.Evaluate(in)
		if got := r.Rule == SelfProtectionRule; got != want {
			t.Errorf("%q: self-protection=%v, want %v (%s)", cmd, got, want, r.Message)
		}
	}
}

// Removing or moving the directory that holds a protected file removes the
// file: `mv ~/.claude ~/.claude.bak` switches the agent's hooks off.
func TestSelfProtectionSeesParentDirectories(t *testing.T) {
	te := newTestEnv(t)
	writeFile(t, te.userFile(), "version: 1\nrules:\n  - {name: yolo, match: {tool: \"*\"}, action: allow}\n")
	writeFile(t, filepath.Join(te.home, ".claude", "settings.json"), "{}")
	writeFile(t, filepath.Join(te.home, ".claude", "commands", "x.md"), "x")
	writeFile(t, filepath.Join(te.repo, ".claude", "settings.json"), "{}")
	writeFile(t, filepath.Join(te.repo, "build", "out.js"), "x")
	writeFile(t, filepath.Join(te.home, "src", "other", "README.md"), "x")
	e, _ := te.load(t)
	wt := filepath.Join(te.agHome, "worktrees", "app", "fix-login")
	writeFile(t, filepath.Join(wt, "main.go"), "x")

	tests := []struct {
		cmd  string
		cwd  string
		want bool
	}{
		{"rm -rf ~/.claude", "", true},
		{"rm -r ~/.claude", "", true},
		{"mv ~/.claude ~/.claude.bak", "", true},
		{"mv -f ~/.claude /tmp/x", "", true},
		{"mv -t /tmp ~/.claude", "", true},
		{"rm -rf ~", "", true},
		{"rm -rf ~/", "", true},
		{"rm -rf $HOME", "", true},
		{"rm -rf /", "", true},
		{"rm -rf ~/.cl*", "", true},
		{"rm -rf ~/{.claude,x}", "", true},
		{"rm -rf .claude", "", true}, // the project's settings
		{"mv .claude /tmp/x", "", true},
		{"rm -rf .", "", true},
		{"rm -rf ../app", "", true},
		{"rm -rf ~/src", "", true},
		{"chmod -R 000 ~/.claude", "", true},
		{"sudo chown -R nobody ~/.claude", "", true},
		{"D=~/.claude; rm -rf $D", "", true},
		{"cd ~ && rm -rf .claude", "", true},
		// Not the tree: a file beside it, a move into it, or a look.
		{"rm ~/.claude", "", false}, // rm without -r can't remove a directory
		{"rm -rf ~/.claude/commands", "", false},
		{"rm -rf build", "", false},
		{"rm -rf ./build ../other", "", false},
		{"rm -rf ~/src/other", "", false},
		{"rm -rf ~/*", "", true}, // names ~/src, which holds the project's settings
		{"mv notes.md ~", "", false},
		{"mv build/out.js ..", "", false},
		{"cp -r ~/.claude /tmp/x", "", false},
		{"ls -R ~", "", false},
		{"chmod 700 ~", "", false},
		{"du -sh ~/.claude", "", false},
		{"rm -rf .", wt, false}, // a task's worktree with no settings of its own
		{"rm -rf build", wt, false},
	}
	for _, tt := range tests {
		in := shell(tt.cmd)
		in.Cwd, in.Repo = te.repo, te.repo
		if tt.cwd != "" {
			in.Cwd, in.Repo = tt.cwd, tt.cwd
		}
		r := e.Evaluate(in)
		if got := r.Rule == SelfProtectionRule; got != tt.want {
			t.Errorf("%q: self-protection=%v, want %v (%s)", tt.cmd, got, tt.want, r.Message)
		}
	}
}

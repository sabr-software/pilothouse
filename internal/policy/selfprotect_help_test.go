package policy

import (
	"testing"
)

// An agent that is held usually asks Pilothouse why: it runs `pilothouse
// policy` or `pilothouse <command> --help`. Those print help and change
// nothing, so they are not held. Anything that could run the command
// instead of its help still is.
func TestSelfProtectionHelp(t *testing.T) {
	te := newTestEnv(t)
	e, _ := te.load(t)

	tests := []struct {
		cmd  string
		want bool // held by self-protection
	}{
		// --- help: not held ---------------------------------------------------
		{"pilothouse policy", false},
		// What an OpenCode session ran when it was held (0.4.14).
		{`pilothouse policy 2>&1 | grep -i -A6 -B6 'opencode\|config\|self' | head -120`, false},
		{"pilothouse policy --help", false},
		{"pilothouse policy -h", false},
		{"pilothouse policy add --help", false},
		{"pilothouse policy preset --help", false},
		{"pilothouse approve --help", false},
		{"pilothouse approve r_1 --help", false},
		{"pilothouse deny -h", false},
		{"pilothouse pair --help", false},
		{"pilothouse remote tailscale --help", false},
		{"pilothouse service --help", false},
		{"pilothouse hooks install --help", false},
		{"pilothouse notify telegram --help 2>&1 | head -40", false},
		{"pilothouse task done --help", false},
		{"pilothouse reply --help", false},
		{"/opt/homebrew/bin/pilothouse policy", false},
		{"cd /tmp && pilothouse policy --help", false},
		{`bash -lc 'pilothouse policy'`, false},
		{`sh -c "pilothouse approve --help"`, false},
		{"pilothouse policy; pilothouse policy rules", false},
		{"pilothouse help approve", false},
		{"pilothouse hooks", false},
		{"pilothouse task", false},

		// --- not help: still held ---------------------------------------------
		// Only groups with nothing to run print help when run bare; the
		// other admin commands do something.
		{"pilothouse here", true},
		{"pilothouse away", true},
		{"pilothouse stop", true},
		{"pilothouse pair", true},
		{"pilothouse devices", true},
		{"pilothouse remote", true},
		{"pilothouse service", true},
		{"pilothouse notify", true},
		{"pilothouse hooks install", true},
		{"pilothouse task done t_1", true},
		{"pilothouse policy add", true},
		{"pilothouse policy trust", true},
		// --help has to be the last word, after plain names only.
		{"pilothouse approve --help r_1", true},
		{"pilothouse approve --help=false r_1", true},
		{"pilothouse approve r_1 --help=false", true},
		{"pilothouse approve --help --help=false r_1", true},
		{"pilothouse approve -h=false r_1", true},
		{"pilothouse approve --yes --help", true},
		{"pilothouse deny r_1 --message --help", true}, // --help is the reason
		{"pilothouse deny r_1 -m --help", true},
		{"pilothouse answer -- --help", true},
		{"pilothouse approve -- -h", true},
		{"pilothouse approve --helpx", true},
		{"pilothouse approve help", true},
		{`pilothouse approve "r 1" --help`, true},
		{`pilothouse approve '' --help`, true},
		{`pilothouse policy ''`, true},
		{`pilothouse policy ""`, true},
		{`pilothouse policy '' add --name x --command 'rm *' --action allow`, true},
		// The shell joins these into --help=false.
		{`pilothouse approve r_1 --help""=false`, true},
		{`pilothouse approve r_1 --help''=false`, true},
		{"pilothouse approve r_1 --help\\\n=false", true},
		{`pilothouse approve r_1 --help$X`, true},
		{`pilothouse approve r_1 --help${X}`, true},
		{`pilothouse approve r_1 "--help$X"`, true},
		// Words only known at run time.
		{`pilothouse policy $SUB`, true},
		{`pilothouse policy "$@"`, true},
		{`pilothouse policy $(echo add) --name x`, true},
		{"pilothouse policy `echo add` --name x", true},
		{`pilothouse policy "$(echo add)"`, true},
		{`pilothouse policy <(echo add)`, true},
		{`pilothouse policy *`, true},
		{`pilothouse policy {add,x}`, true},
		{`pilothouse policy ~`, true},
		{`pilothouse approve $ID --help`, true},
		{`pilothouse approve * --help`, true},
		{`pilothouse approve r_{1,2} --help`, true},
		{"pilothouse policy \\\n add --name x --command 'rm *' --action allow", true},
		{"pilothouse policy\\\n add --name x", true},
		// xargs and find add the words that matter after the ones written.
		{"echo add | xargs pilothouse policy", true},
		{"echo r_1 | xargs pilothouse approve --help", true},
		{"echo --help=false r_1 | xargs pilothouse approve --help", true},
		{`find . -name x -exec pilothouse policy {} \;`, true},
		{`find . -name x -exec pilothouse approve --help {} +`, true},
		{`find . -name x -exec pilothouse approve --help + --help=false r_1 \;`, true},
		{`find . -name x -exec pilothouse policy + add --name x \;`, true},
		// A chain is held when any part of it is.
		{"pilothouse policy && pilothouse approve r_1", true},
		{"pilothouse approve --help; pilothouse approve r_1", true},
		{"pilothouse policy --help | pilothouse deny r_1", true},
		// Text fed to an interpreter keeps the whole-line scan, which does
		// not try to tell help from the rest.
		{`python3 -c "import os; os.system('pilothouse approve --help')"`, true},
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

func TestHelpOnly(t *testing.T) {
	tests := []struct {
		cmd  string
		rest []string
		want bool
	}{
		{"policy", nil, true},
		{"policy", []string{}, true},
		{"hooks", nil, true},
		{"task", nil, true},
		{"service", nil, false},
		{"approve", nil, false},
		{"here", nil, false},
		{"approve", []string{"--help"}, true},
		{"approve", []string{"-h"}, true},
		{"approve", []string{"r_1", "--help"}, true},
		{"policy", []string{"preset", "guarded-autopilot", "on", "--help"}, true},
		{"approve", []string{"--help", "r_1"}, false},
		{"approve", []string{"-y", "--help"}, false},
		{"approve", []string{"", "--help"}, false},
		{"approve", []string{"--", "--help"}, false},
		{"approve", []string{"--help=true"}, false},
		{"approve", []string{"-help"}, false},
		{"approve", []string{"r 1", "--help"}, false},
		{"approve", []string{"$X", "--help"}, false},
		{"approve", []string{"a=b", "--help"}, false},
		{"approve", []string{"a/b", "--help"}, false},
		{"policy", []string{""}, false},
	}
	for _, tt := range tests {
		if got := helpOnly(tt.cmd, tt.rest); got != tt.want {
			t.Errorf("helpOnly(%q, %q) = %v, want %v", tt.cmd, tt.rest, got, tt.want)
		}
	}
}

// Every command that is help when run bare must be an admin command, or the
// entry does nothing.
func TestHelpGroupsAreAdminCommands(t *testing.T) {
	for _, g := range HelpGroups() {
		if !adminCommands[g] {
			t.Errorf("helpGroups has %q, which is not an admin command", g)
		}
	}
}

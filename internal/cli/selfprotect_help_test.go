package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/sabr-software/pilothouse/internal/policy"
)

// The self-protection rule does not hold `pilothouse <admin command> …
// --help` or a bare command group, because they print help and do nothing
// (internal/policy/selfprotect.go, helpOnly). That is true only while these
// commands parse flags the usual way, so this test runs every one of them in
// the real command tree, with each command's work replaced by a recorder,
// and fails if anything runs.

// stubTree replaces what every command does with a note that it ran.
func stubTree(root *cobra.Command, ran *string) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() {
			c.Run = nil
			c.RunE = func(cmd *cobra.Command, _ []string) error {
				*ran = cmd.CommandPath()
				return nil
			}
		}
		c.PersistentPreRun, c.PersistentPreRunE = nil, nil
		c.PreRun, c.PreRunE = nil, nil
		c.PostRun, c.PostRunE = nil, nil
		c.PersistentPostRun, c.PersistentPostRunE = nil, nil
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// runStubbed runs args against a fresh command tree and reports which
// command ran ("" for none) and what was printed.
func runStubbed(args ...string) (ran, out string) {
	root := newRoot()
	stubTree(root, &ran)
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	_ = root.Execute()
	return ran, buf.String()
}

func childNamed(c *cobra.Command, name string) *cobra.Command {
	for _, sub := range c.Commands() {
		if sub.Name() == name || sub.HasAlias(name) {
			return sub
		}
	}
	return nil
}

// adminPaths are the admin commands and all their subcommands, as argument
// lists: ["policy"], ["policy", "add"], …
func adminPaths(t *testing.T) [][]string {
	t.Helper()
	root := newRoot()
	var paths [][]string
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		paths = append(paths, path)
		for _, sub := range c.Commands() {
			walk(sub, append(slices.Clone(path), sub.Name()))
		}
	}
	for _, name := range policy.AdminCommands() {
		c := childNamed(root, name)
		if c == nil {
			t.Errorf("self-protection lists the admin command %q, which pilothouse does not have", name)
			continue
		}
		walk(c, []string{name})
	}
	return paths
}

func TestAdminHelpRunsNothing(t *testing.T) {
	paths := adminPaths(t)
	if len(paths) < len(policy.AdminCommands()) {
		t.Fatalf("only %d admin command paths found", len(paths))
	}
	for _, path := range paths {
		for _, tail := range [][]string{
			{"--help"},
			{"-h"},
			{"r_1", "--help"},
			{"s_1", "some.name", "x:y", "-h"},
		} {
			args := append(slices.Clone(path), tail...)
			ran, out := runStubbed(args...)
			if ran != "" {
				t.Errorf("pilothouse %s ran %q; the self-protection rule lets it through as help", strings.Join(args, " "), ran)
				continue
			}
			if len(tail) == 1 && !strings.Contains(out, "Usage:") {
				t.Errorf("pilothouse %s printed no help:\n%s", strings.Join(args, " "), out)
			}
		}
	}
}

// A stubbed command does run when it is not asked for help: without this the
// test above would pass for the wrong reason.
func TestStubbedAdminCommandsRun(t *testing.T) {
	for _, args := range [][]string{
		{"approve", "r_1"},
		{"here"},
		{"approve", "--help=false", "r_1"},
		{"approve", "--help", "--help=false", "r_1"},
		{"deny", "r_1", "--message", "--help"}, // --help is the reason
	} {
		if ran, out := runStubbed(args...); ran == "" {
			t.Errorf("pilothouse %s ran nothing:\n%s", strings.Join(args, " "), out)
		}
	}
}

// helpGroups in the policy package are the admin commands that print help
// when run with nothing after them. Each must have nothing to run, and every
// admin command with nothing to run should be listed, so that an agent
// looking for help is not held.
func TestHelpGroupsMatchCommands(t *testing.T) {
	root := newRoot()
	groups := policy.HelpGroups()
	for _, name := range groups {
		c := childNamed(root, name)
		if c == nil {
			t.Errorf("helpGroups lists %q, which pilothouse does not have", name)
			continue
		}
		if c.Runnable() {
			t.Errorf("helpGroups lists %q, but `pilothouse %s` does something when run bare", name, name)
		}
		if ran, out := runStubbed(name); ran != "" || !strings.Contains(out, "Usage:") {
			t.Errorf("`pilothouse %s` ran %q and printed:\n%s", name, ran, out)
		}
	}
	for _, name := range policy.AdminCommands() {
		if c := childNamed(root, name); c != nil && !c.Runnable() && !slices.Contains(groups, name) {
			t.Errorf("`pilothouse %s` only prints help when run bare; add it to helpGroups in internal/policy/selfprotect.go", name)
		}
	}
}

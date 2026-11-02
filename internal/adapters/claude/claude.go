// Package claude adapts Claude Code: it parses Claude Code's hook payloads,
// answers them in its hook protocol and wires `pilothouse hook claude` into
// ~/.claude/settings.json.
//
// Reference: https://code.claude.com/docs/en/hooks (hook events, stdin
// fields, matcher syntax, exit codes, PreToolUse permissionDecision and
// PermissionRequest decision.behavior outputs). docs/AGENTS.md summarizes
// what Pilothouse relies on.
package claude

import (
	"os"
	"path/filepath"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/adapters/hookkit"
	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/paths"
)

// PermissionTimeout is the hook timeout written for PermissionRequest, in
// seconds; Claude Code kills the hook after it. It is also Claude Code's
// default command-hook timeout, written explicitly so a changed default
// cannot shorten our hold.
const PermissionTimeout = 600

// PreToolTimeout bounds PreToolUse: the daemon answers in milliseconds,
// and a wedged hook must not stall the agent for the 600 s default.
const PreToolTimeout = 30

// Adapter is Claude Code.
type Adapter struct{}

// New returns the Claude Code adapter.
func New() adapters.Adapter { return Adapter{} }

func (Adapter) Kind() api.AgentKind { return api.AgentClaude }
func (Adapter) DisplayName() string { return "Claude Code" }

// ProcessNames: the native installer and npm both run a binary invoked as
// `claude` (the npm package's file is claude.exe, linked as claude).
func (Adapter) ProcessNames() []string { return []string{"claude", "claude.exe"} }

// SettingsPath is the user settings file hooks are installed into.
func SettingsPath() string {
	return filepath.Join(paths.UserHome(), ".claude", "settings.json")
}

func (Adapter) Detect() adapters.Detection {
	d := adapters.Detection{SettingsPath: SettingsPath()}
	d.Binary, d.Version = hookkit.DetectBinary("claude")
	_, err := os.Stat(filepath.Dir(d.SettingsPath))
	d.Installed = d.Binary != "" || err == nil
	if d.Binary == "" && d.Installed {
		d.Note = "~/.claude exists but `claude` is not on PATH"
	}
	return d
}

// MaxHold: Claude Code kills a PermissionRequest hook after its timeout
// (we install 600 s); nothing else may block.
func (Adapter) MaxHold(kind adapters.HookKind) time.Duration {
	if kind == adapters.HookPermission {
		return PermissionTimeout * time.Second
	}
	return 0
}

// ProtectedPaths are Claude Code settings files: whoever can edit them can
// add a hook that approves everything. Absolute entries are the user's own
// files; "**/" entries are glob patterns for project and local settings in
// any repository.
func (Adapter) ProtectedPaths() []string {
	dir := filepath.Join(paths.UserHome(), ".claude")
	return []string{
		filepath.Join(dir, "settings.json"),
		filepath.Join(dir, "settings.local.json"),
		"**/.claude/settings.json",
		"**/.claude/settings.local.json",
	}
}

func (Adapter) ResumeArgs(agentSessionID string) []string {
	if agentSessionID == "" {
		return nil
	}
	return []string{"claude", "--resume", agentSessionID}
}

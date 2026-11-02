package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

const existingSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "opus",
  "permissions": {
    "allow": [
      "Bash(npm test:*)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "~/.claude/hooks/block-rm.sh"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say done"
          }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "~/.claude/statusline.sh"
  }
}
`

func writeSettings(t *testing.T, home, content string) string {
	t.Helper()
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeShim(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bin", "pilothouse")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func backups(t *testing.T, settings string) []string {
	t.Helper()
	m, _ := filepath.Glob(settings + ".pilothouse-bak-*")
	return m
}

func TestInstallRoundTrip(t *testing.T) {
	home := isolate(t)
	path := writeSettings(t, home, existingSettings)
	shim := fakeShim(t)
	a := Adapter{}
	opts := adapters.InstallOptions{ShimPath: shim}

	// Dry run: a diff, no write.
	dry, err := a.InstallHooks(adapters.InstallOptions{ShimPath: shim, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.Changed || dry.Backup != "" || dry.Before != existingSettings || !strings.Contains(dry.After, shim+" hook claude") {
		t.Fatalf("dry run: %+v", dry)
	}
	if b, _ := os.ReadFile(path); string(b) != existingSettings || len(backups(t, path)) != 0 {
		t.Fatal("dry run wrote")
	}

	ch, err := a.InstallHooks(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Changed || ch.Backup == "" {
		t.Fatalf("install: %+v", ch)
	}
	if b, _ := os.ReadFile(ch.Backup); string(b) != existingSettings {
		t.Error("backup is not the original")
	}
	after, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "opus" || doc["statusLine"] == nil || doc["$schema"] == nil || doc["permissions"] == nil {
		t.Errorf("unrelated keys lost:\n%s", after)
	}
	hooks := doc["hooks"].(map[string]any)
	pre := hooks["PreToolUse"].([]any)
	if len(pre) != 2 || !strings.Contains(string(after), "block-rm.sh") || !strings.Contains(string(after), "say done") {
		t.Errorf("user hooks lost:\n%s", after)
	}
	ours := pre[1].(map[string]any)
	h := ours["hooks"].([]any)[0].(map[string]any)
	if ours["matcher"] != "*" || h["timeout"] != 30.0 || h["command"] != shim+" hook claude" {
		t.Errorf("PreToolUse handler: %v", ours)
	}
	perm := hooks["PermissionRequest"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if perm["timeout"] != 600.0 {
		t.Errorf("PermissionRequest timeout: %v", perm)
	}
	for _, w := range wiredEvents {
		if hooks[w.event] == nil {
			t.Errorf("%s not wired", w.event)
		}
	}

	st, err := a.Status(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Current || len(st.Events) != len(wiredEvents) || len(st.Problems) != 0 {
		t.Errorf("status after install: %+v", st)
	}

	// Idempotent: no change, no new backup.
	again, err := a.InstallHooks(opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed || again.Backup != "" || len(backups(t, path)) != 1 {
		t.Errorf("second install: %+v", again)
	}

	un, err := a.UninstallHooks(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !un.Changed || un.Backup == "" {
		t.Fatalf("uninstall: %+v", un)
	}
	restored, _ := os.ReadFile(path)
	var gotDoc, wantDoc any
	_ = json.Unmarshal(restored, &gotDoc)
	_ = json.Unmarshal([]byte(existingSettings), &wantDoc)
	if !jsonEqual(gotDoc, wantDoc) {
		t.Errorf("uninstall did not restore the original:\n%s", restored)
	}
	if st, _ := a.Status(opts); st.Installed || st.Current {
		t.Errorf("status after uninstall: %+v", st)
	}
	if un2, _ := a.UninstallHooks(opts); un2.Changed {
		t.Error("second uninstall changed")
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestInstallCreatesMissingSettings(t *testing.T) {
	home := isolate(t)
	a := Adapter{}
	ch, err := a.InstallHooks(adapters.InstallOptions{ShimPath: fakeShim(t)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if !ch.Changed || ch.Backup != "" || ch.Path != path {
		t.Errorf("%+v", ch)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("created file: %v %v", fi, err)
	}
	// Uninstalling everything from a file we created leaves valid JSON.
	if _, err := a.UninstallHooks(adapters.InstallOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.TrimSpace(string(b)) != "{}" {
		t.Errorf("after uninstall: %q", b)
	}
	// Uninstall with no settings file at all is a no-op, not a create.
	os.Remove(path)
	if ch, err := a.UninstallHooks(adapters.InstallOptions{}); err != nil || ch.Changed {
		t.Errorf("%+v %v", ch, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("uninstall created the file")
	}
}

func TestInstallRefusesInvalidJSON(t *testing.T) {
	home := isolate(t)
	bad := "{\n  // comments are not JSON\n  \"model\": \"opus\"\n}\n"
	path := writeSettings(t, home, bad)
	a := Adapter{}
	if _, err := a.InstallHooks(adapters.InstallOptions{ShimPath: fakeShim(t)}); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.UninstallHooks(adapters.InstallOptions{}); err == nil {
		t.Fatal("uninstall accepted invalid JSON")
	}
	if b, _ := os.ReadFile(path); string(b) != bad || len(backups(t, path)) != 0 {
		t.Error("invalid file was touched")
	}
	st, err := a.Status(adapters.InstallOptions{})
	if err != nil || st.Installed || len(st.Problems) == 0 {
		t.Errorf("status on invalid file: %+v %v", st, err)
	}
}

func TestInstallMovedShim(t *testing.T) {
	home := isolate(t)
	writeSettings(t, home, existingSettings)
	a := Adapter{}
	old := fakeShim(t)
	if _, err := a.InstallHooks(adapters.InstallOptions{ShimPath: old}); err != nil {
		t.Fatal(err)
	}
	os.Remove(old)
	newShim := fakeShim(t)
	st, _ := a.Status(adapters.InstallOptions{ShimPath: newShim})
	if !st.Installed || st.Current || len(st.Problems) == 0 || !strings.Contains(st.Problems[0], "does not exist") {
		t.Errorf("status with moved binary: %+v", st)
	}
	ch, err := a.InstallHooks(adapters.InstallOptions{ShimPath: newShim})
	if err != nil || !ch.Changed {
		t.Fatalf("%+v %v", ch, err)
	}
	if strings.Contains(ch.After, old) || strings.Count(ch.After, "hook claude") != len(wiredEvents) {
		t.Errorf("stale hooks remain:\n%s", ch.After)
	}
}

func TestStatusDisableAllHooks(t *testing.T) {
	home := isolate(t)
	writeSettings(t, home, `{"disableAllHooks": true}`)
	a := Adapter{}
	shim := fakeShim(t)
	if _, err := a.InstallHooks(adapters.InstallOptions{ShimPath: shim}); err != nil {
		t.Fatal(err)
	}
	st, _ := a.Status(adapters.InstallOptions{ShimPath: shim})
	if st.Current || len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "disableAllHooks") {
		t.Errorf("%+v", st)
	}
}

func TestInstallQuotesShimPath(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "My Tools")
	shim := filepath.Join(dir, "pilothouse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := Adapter{}
	ch, err := a.InstallHooks(adapters.InstallOptions{ShimPath: shim})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ch.After, `"'`+shim+`' hook claude"`) {
		t.Errorf("not quoted:\n%s", ch.After)
	}
	st, _ := a.Status(adapters.InstallOptions{ShimPath: shim})
	if !st.Current {
		t.Errorf("quoted install not recognized: %+v", st)
	}
}

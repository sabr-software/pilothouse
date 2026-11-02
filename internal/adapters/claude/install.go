package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
	"github.com/sabr-software/pilothouse/internal/adapters/hookkit"
)

// owned recognizes hook commands we installed (`… pilothouse hook claude`).
var owned = hookkit.OwnedBy("claude")

// wiredEvents are the Claude Code events Pilothouse listens to, with the
// matcher and timeout written for each. Tool events match every tool
// ("*"); the others omit the matcher (all occurrences). Only PreToolUse
// and PermissionRequest get explicit timeouts: the shim bounds every other
// call to 2 s itself, and a timeout on SessionEnd would stretch Claude
// Code's exit budget for every SessionEnd hook.
var wiredEvents = []struct {
	event, matcher string
	timeout        int
}{
	{"SessionStart", "", 0},
	{"SessionEnd", "", 0},
	{"UserPromptSubmit", "", 0},
	{"PreToolUse", "*", PreToolTimeout},
	{"PermissionRequest", "*", PermissionTimeout},
	{"PostToolUse", "*", 0},
	{"PostToolUseFailure", "*", 0},
	{"Notification", "", 0},
	{"Stop", "", 0},
	{"SubagentStop", "", 0},
}

func specs(shimPath string) []hookkit.Spec {
	cmd := hookkit.ShimCommand(shimPath, "claude")
	out := make([]hookkit.Spec, len(wiredEvents))
	for i, w := range wiredEvents {
		out[i] = hookkit.Spec{Event: w.event, Matcher: w.matcher, Command: cmd, Timeout: w.timeout}
	}
	return out
}

// InstallHooks merges our handlers into ~/.claude/settings.json, keeping
// every other key and hook. Idempotent; backs the file up before writing;
// refuses to touch a file that is not valid JSON.
func (Adapter) InstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	if opts.ShimPath == "" {
		return adapters.Change{}, fmt.Errorf("install: no pilothouse binary path")
	}
	plan, err := hookkit.PlanJSON(SettingsPath(), specs(opts.ShimPath), owned)
	if err != nil {
		return adapters.Change{}, err
	}
	summary := fmt.Sprintf("Claude Code hooks are up to date in %s", SettingsPath())
	if plan.Result.Changed {
		summary = fmt.Sprintf("Wire %d Claude Code hook events to %s", len(wiredEvents), hookkit.ShimCommand(opts.ShimPath, "claude"))
	}
	return commit(plan, summary, opts.DryRun)
}

// UninstallHooks removes every handler we installed and nothing else.
func (Adapter) UninstallHooks(opts adapters.InstallOptions) (adapters.Change, error) {
	plan, err := hookkit.PlanJSON(SettingsPath(), nil, owned)
	if err != nil {
		return adapters.Change{}, err
	}
	summary := "No Pilothouse hooks in " + SettingsPath()
	if plan.Result.Changed {
		summary = fmt.Sprintf("Remove %d Pilothouse hook handlers from %s", len(plan.Result.Found), SettingsPath())
	}
	return commit(plan, summary, opts.DryRun)
}

func commit(plan hookkit.Plan, summary string, dryRun bool) (adapters.Change, error) {
	ch := plan.Change(summary)
	if dryRun {
		return ch, nil
	}
	return ch, plan.Commit(&ch, time.Now())
}

// Status reports which events are wired and whether they match what
// InstallHooks would write for opts.ShimPath (any pilothouse path counts
// when ShimPath is empty).
func (Adapter) Status(opts adapters.InstallOptions) (adapters.HookStatus, error) {
	st := adapters.HookStatus{Path: SettingsPath()}
	plan, err := hookkit.PlanJSON(st.Path, specs(opts.ShimPath), owned)
	if err != nil {
		st.Problems = append(st.Problems, err.Error())
		return st, nil
	}
	have := map[string]bool{}
	for _, f := range plan.Result.Found {
		if !have[f.Event] {
			st.Events = append(st.Events, f.Event)
		}
		have[f.Event] = true
	}
	st.Installed = len(st.Events) > 0
	st.Problems = hookkit.Problems(plan.Result.Found)
	if hooksDisabled(plan.File.Data) {
		st.Problems = append(st.Problems, `"disableAllHooks": true in settings.json turns every hook off, Pilothouse's included`)
	}
	var missing []string
	for _, w := range wiredEvents {
		if !have[w.event] {
			missing = append(missing, w.event)
		}
	}
	if st.Installed && len(missing) > 0 {
		st.Problems = append(st.Problems, "not wired: "+strings.Join(missing, ", ")+"; run `pilothouse hooks install`")
	}
	current := !plan.Result.Changed
	if opts.ShimPath == "" {
		current = len(missing) == 0
	}
	st.Current = st.Installed && current && len(st.Problems) == 0
	return st, nil
}

func hooksDisabled(doc []byte) bool {
	var v struct {
		DisableAllHooks bool `json:"disableAllHooks"`
	}
	return json.Unmarshal(doc, &v) == nil && v.DisableAllHooks
}

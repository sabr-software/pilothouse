package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/policy"
)

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	rf, err := openRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 39)
	line = append(line, '\n') // 40 bytes: two lines fit in 100, a third rotates
	for i := 0; i < 20; i++ {
		if _, err := rf.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := rf.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Write(line); err == nil {
		t.Fatal("write after close succeeded")
	}
	for _, name := range []string{"daemon.log", "daemon.log.1", "daemon.log.2", "daemon.log.3"} {
		fi, err := os.Stat(filepath.Join(filepath.Dir(path), name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Size() > 100 || fi.Size() == 0 {
			t.Errorf("%s has %d bytes", name, fi.Size())
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", name, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("kept more than 3 old logs: %v", err)
	}

	// Reopening appends to the existing file and keeps counting its size.
	rf, err = openRotatingFile(path, 100, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if rf.size == 0 {
		t.Fatal("size of existing log not picked up")
	}
}

// `pilothouse log` renders policy.decision events, whose data is
// policy.DecisionEvent as the daemon emits it.
func TestFormatEventLinePolicyDecision(t *testing.T) {
	de := policy.DecisionEvent{ToolUseID: "tu-1", Tool: "Bash", Summary: "Run: git push -f",
		Result:  policy.Result{Action: api.BehaviorDeny, Rule: "no-force-push", Message: "Open a PR instead."},
		Outcome: api.BehaviorDeny}
	data, _ := json.Marshal(de)
	line := FormatEventLine(api.Event{Seq: 1, Type: api.EventPolicyDecision, Data: data})
	for _, want := range []string{"rule=no-force-push", "action=deny", "outcome=deny", "summary=Run: git push -f", "message=Open a PR instead."} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	data, _ = json.Marshal(policy.DecisionEvent{Tool: "Bash", Grant: "g_abc", Outcome: api.BehaviorAllow})
	if line := FormatEventLine(api.Event{Type: api.EventPolicyDecision, Data: data}); !strings.Contains(line, "grant=g_abc") ||
		!strings.Contains(line, "outcome=allow") {
		t.Errorf("grant line = %q", line)
	}
}

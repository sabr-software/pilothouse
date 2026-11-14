package cli

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// --json keeps the values: what a script reads back is what the daemon
// sent, with nothing a terminal acts on in between.
func TestJSONOutputKeepsTheValues(t *testing.T) {
	hstStart(t, hstGen{enums: hstEnums(api.CheckpointPermission)})
	out, err := ibxRun(t, "", "ls", "--json")
	if err != nil {
		t.Fatal(err)
	}
	hstInert(t, "ls --json", out)
	var got lsOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if len(got.Requests) == 0 || got.Requests[0].Command != "Command"+hostile {
		t.Fatalf("the command changed on the way: %+v", got.Requests)
	}
}

func TestForPrint(t *testing.T) {
	type inner struct {
		Text string
		at   time.Time
	}
	v := struct {
		State api.State
		List  []inner
		Ptr   *inner
		Map   map[string][]string
		Any   map[string]any
		Raw   json.RawMessage
		At    time.Time
	}{
		State: api.State("blocked\x1b[2K"),
		List:  []inner{{Text: hostile, at: ibxT0}},
		Ptr:   &inner{Text: "two\nlines"},
		Map:   map[string][]string{"k\x1b": {"v\u009b"}},
		Any:   map[string]any{"s": "a\x07", "n": 1.5, "l": []any{"b\x7f"}},
		Raw:   json.RawMessage("{\"a\":\"\x7f\"}"),
		At:    ibxT0,
	}
	forPrint(&v)
	hstInert(t, "forPrint", string(v.State)+v.List[0].Text+v.Ptr.Text)
	if v.State != "blocked␛[2K" || v.Ptr.Text != "two lines" || v.Map["k␛"][0] != "v<U+009B>" ||
		v.Any["s"] != "a␇" || v.Any["n"] != 1.5 || v.Any["l"].([]any)[0] != "b␡" {
		t.Errorf("%+v", v)
	}
	if len(v.Map) != 1 || len(v.Any) != 3 {
		t.Errorf("keys: %v %v", v.Map, v.Any)
	}
	if string(v.Raw) != "{\"a\":\"\x7f\"}" || !v.At.Equal(ibxT0) || !v.List[0].at.Equal(ibxT0) {
		t.Errorf("raw JSON and times are not text: %q %v", v.Raw, v.At)
	}
}

// A patch stays the patch in a pipe; on a terminal its control
// characters are shown.
func TestRawOut(t *testing.T) {
	saved := outIsTerminal
	t.Cleanup(func() { outIsTerminal = saved })
	patch := "+echo \x1b]52;c;ZXZpbA==\x07\r\n+caf\u00e9\tend\r"
	for _, tty := range []bool{false, true} {
		outIsTerminal = func(io.Writer) bool { return tty }
		var b strings.Builder
		out, done := rawOut(&b)
		for i := 0; i < len(patch); i += 3 {
			if _, err := io.WriteString(out, patch[i:min(i+3, len(patch))]); err != nil {
				t.Fatal(err)
			}
		}
		if err := done(); err != nil {
			t.Fatal(err)
		}
		switch {
		case !tty && b.String() != patch:
			t.Errorf("piped: %q", b.String())
		case tty && b.String() != "+echo ␛]52;c;ZXZpbA==␇\n+caf\u00e9\tend␍":
			t.Errorf("terminal: %q", b.String())
		}
	}
}

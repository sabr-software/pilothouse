package transcript

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

func TestReadOpenCode(t *testing.T) {
	db, path := openTestOpenCode(t)
	sid := "ses_test"
	mustExec(t, db, `INSERT INTO session
		(id, cost, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sid, 0.25, 100, 20, 5, 30, 0, `{"id":"model-x","providerID":"p"}`)
	mustExec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"m1", sid, 1000, 1000, `{"role":"user"}`)
	mustExec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		"m2", sid, 1100, 1500, `{"role":"assistant"}`)
	part(t, db, "p1", "m1", sid, 1000, 1000, `{"type":"text","text":"hello"}`)
	part(t, db, "p2", "m2", sid, 1100, 1100, `{"type":"text","text":"hi there"}`)
	part(t, db, "p3", "m2", sid, 1200, 1200,
		`{"type":"tool","tool":"bash","state":{"status":"running","input":{"command":"npm test"}}}`)
	part(t, db, "p4", "m2", sid, 1300, 1300,
		`{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"/tmp/x/file.go"},"output":"package x"}}`)

	res, err := ReadOpenCode(filepath.Join(t.TempDir(), "missing.db"), sid, "", "", 10, time.Now())
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("missing db: %v", err)
	}

	res, err = ReadOpenCode(path, sid, "/tmp/x", "", 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 4 {
		t.Fatalf("items %d: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].Kind != api.ActivityPrompt || res.Items[0].Text != "hello" || res.Items[0].Turn != 1 {
		t.Fatalf("prompt %+v", res.Items[0])
	}
	if res.Items[1].Kind != api.ActivityText || res.Items[1].Text != "hi there" || res.Items[1].Turn != 1 {
		t.Fatalf("text %+v", res.Items[1])
	}
	if res.Items[2].Summary != "Run: npm test" || !res.Items[2].Pending {
		t.Fatalf("pending tool %+v", res.Items[2])
	}
	if res.Items[3].Summary != "Read: file.go" || res.Items[3].Result != "package x" {
		t.Fatalf("read tool %+v", res.Items[3])
	}
	if res.Usage.InputTokens != 100 || res.Usage.OutputTokens != 20 || res.Usage.CostUSD != 0.25 || res.Usage.Turns != 1 {
		t.Fatalf("usage %+v", res.Usage)
	}

	// Nothing changed: the cursor returns no items.
	res2, err := ReadOpenCode(path, sid, "/tmp/x", res.Cursor, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Items) != 0 {
		t.Fatalf("incremental items %+v", res2.Items)
	}

	// A completed tool updates in place.
	part(t, db, "p3", "m2", sid, 1200, 1600,
		`{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"npm test"},"output":"PASS","metadata":{"exit":0}}}`)
	res3, err := ReadOpenCode(path, sid, "/tmp/x", res.Cursor, 10, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res3.Items) != 1 || res3.Items[0].ID != "oc-p3" || res3.Items[0].Pending || res3.Items[0].Result != "PASS" {
		t.Fatalf("updated tool %+v", res3.Items)
	}
}

func TestReadOpenCodeDays(t *testing.T) {
	db, path := openTestOpenCode(t)
	loc := time.FixedZone("CST", -6*3600)
	day1 := time.Date(2026, 11, 1, 23, 0, 0, 0, loc).UnixMilli() // 05:00 UTC on 2 Nov
	day2 := time.Date(2026, 11, 2, 9, 0, 0, 0, loc).UnixMilli()
	mustExec(t, db, `INSERT INTO session (id) VALUES ('ses_main'), ('ses_other')`)
	mustExec(t, db, `INSERT INTO session (id, parent_id) VALUES ('ses_child', 'ses_main')`)
	msg := func(id, sid string, at int64, data string) {
		mustExec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`, id, sid, at, at, data)
	}
	asst := func(model string, cost float64, in, out, reason, cr, cw int) string {
		return fmt.Sprintf(`{"role":"assistant","modelID":%q,"cost":%v,"tokens":{"input":%d,"output":%d,"reasoning":%d,"cache":{"read":%d,"write":%d}}}`,
			model, cost, in, out, reason, cr, cw)
	}
	msg("m1", "ses_main", day1, `{"role":"user"}`)
	msg("m2", "ses_main", day1+1000, asst("deepseek-v4.1-flash", 0.002, 10, 5, 3, 1000, 0))
	msg("m3", "ses_child", day1+2000, `{"role":"user"}`) // a subagent's task: not a turn
	msg("m4", "ses_child", day1+3000, asst("deepseek-v4.1-flash", 0.001, 20, 1, 0, 500, 0))
	msg("m5", "ses_main", day2, `{"role":"user"}`)
	msg("m6", "ses_main", day2+1000, asst("qwen3.6:27b", 0, 7, 7, 0, 0, 0))
	msg("m7", "ses_other", day2, asst("deepseek-v4.1-flash", 9, 999, 999, 0, 0, 0))
	msg("m8", "ses_main", day2+2000, `not json`)

	days, err := ReadOpenCodeDays(path, "ses_main", loc)
	if err != nil {
		t.Fatal(err)
	}
	want := []DayUsage{
		{Day: "2026-11-01", Model: "deepseek-v4.1-flash", Tokens: Tokens{Input: 30, Output: 6, Reasoning: 3, CacheRead: 1500}, CostUSD: 0.003, Turns: 1},
		{Day: "2026-11-02", Model: "qwen3.6:27b", Tokens: Tokens{Input: 7, Output: 7}, Turns: 1},
	}
	if len(days) != len(want) {
		t.Fatalf("days = %+v", days)
	}
	for i := range want {
		g := days[i]
		if g.Day != want[i].Day || g.Model != want[i].Model || g.Tokens != want[i].Tokens || g.Turns != want[i].Turns ||
			math.Abs(g.CostUSD-want[i].CostUSD) > 1e-9 {
			t.Errorf("day %d = %+v, want %+v", i, g, want[i])
		}
	}
	if _, err := ReadOpenCodeDays(filepath.Join(t.TempDir(), "none.db"), "ses_main", loc); !IsMissing(err) {
		t.Errorf("missing database: err = %v", err)
	}
}

func openTestOpenCode(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, parent_id TEXT, cost REAL, tokens_input INTEGER, tokens_output INTEGER,
			tokens_reasoning INTEGER, tokens_cache_read INTEGER, tokens_cache_write INTEGER, model TEXT)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		mustExec(t, db, q)
	}
	return db, path
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func part(t *testing.T, db *sql.DB, id, msg, sid string, created, updated int64, data string) {
	t.Helper()
	mustExec(t, db, `INSERT OR REPLACE INTO part (id, message_id, session_id, time_created, time_updated, data)
		VALUES (?, ?, ?, ?, ?, ?)`, id, msg, sid, created, updated, data)
}

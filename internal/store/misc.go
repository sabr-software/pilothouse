package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// ---- grants ----

// InsertGrant stores an "allow for this session" grant.
func (tx *Tx) InsertGrant(ctx context.Context, g api.Grant) error {
	_, err := tx.q.ExecContext(ctx,
		`INSERT INTO grants(id, session_id, tool, prefix, decision_by, created_at) VALUES(?, ?, ?, ?, ?, ?)`,
		g.ID, g.SessionID, g.Tool, g.Prefix, g.DecisionBy, nanos(g.CreatedAt))
	if err != nil {
		return fmt.Errorf("insert grant: %w", err)
	}
	return nil
}

// Grants returns a session's grants, oldest first.
func (r Reader) Grants(ctx context.Context, sessionID string) ([]api.Grant, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT id, session_id, tool, prefix, decision_by, created_at FROM grants WHERE session_id = ? ORDER BY created_at, id`,
		sessionID)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()
	out := []api.Grant{}
	for rows.Next() {
		var g api.Grant
		var at int64
		if err := rows.Scan(&g.ID, &g.SessionID, &g.Tool, &g.Prefix, &g.DecisionBy, &at); err != nil {
			return nil, fmt.Errorf("list grants: %w", err)
		}
		g.CreatedAt = time.Unix(0, at).UTC()
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---- tasks ----

// PutTask inserts or replaces a task.
func (tx *Tx) PutTask(ctx context.Context, t api.Task) error {
	if t.ID == "" {
		return errors.New("put task: empty id")
	}
	b, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("put task %s: %w", t.ID, err)
	}
	_, err = tx.q.ExecContext(ctx, `
INSERT INTO tasks(id, session_id, status, updated_at, data) VALUES(?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET session_id = excluded.session_id, status = excluded.status,
	updated_at = excluded.updated_at, data = excluded.data`,
		t.ID, t.SessionID, string(t.Status), nanos(t.UpdatedAt), string(b))
	if err != nil {
		return fmt.Errorf("put task %s: %w", t.ID, err)
	}
	return nil
}

// Task returns the task with exactly this id.
func (r Reader) Task(ctx context.Context, id string) (api.Task, error) {
	var data string
	err := r.q.QueryRowContext(ctx, `SELECT data FROM tasks WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Task{}, ErrNotFound
	}
	if err != nil {
		return api.Task{}, fmt.Errorf("read task: %w", err)
	}
	var t api.Task
	if err := json.Unmarshal([]byte(data), &t); err != nil {
		return api.Task{}, fmt.Errorf("decode task: %w", err)
	}
	return t, nil
}

// ResolveTaskID expands an id or unique prefix.
func (r Reader) ResolveTaskID(ctx context.Context, prefix string) (string, error) {
	return r.resolveID(ctx, "tasks", "t_", prefix)
}

// Tasks returns tasks updated at or after since (zero = all), newest first.
func (r Reader) Tasks(ctx context.Context, since time.Time) ([]api.Task, error) {
	q := `SELECT data FROM tasks`
	var args []any
	if !since.IsZero() {
		q += ` WHERE updated_at >= ? OR status IN (?, ?)`
		args = append(args, nanos(since), string(api.TaskRunning), string(api.TaskReview))
	}
	rows, err := r.q.QueryContext(ctx, q+` ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()
	out := []api.Task{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		var t api.Task
		if err := json.Unmarshal([]byte(data), &t); err != nil {
			return nil, fmt.Errorf("decode task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- evidence ----

// AddEvidence records one piece of state evidence for a session.
func (tx *Tx) AddEvidence(ctx context.Context, sessionID string, e api.StateEvidence) error {
	_, err := tx.q.ExecContext(ctx,
		`INSERT INTO evidence(session_id, at, layer, state, kind, detail) VALUES(?, ?, ?, ?, ?, ?)`,
		sessionID, nanos(e.At), string(e.Layer), string(e.State), string(e.Kind), e.Detail)
	if err != nil {
		return fmt.Errorf("add evidence: %w", err)
	}
	return nil
}

// Evidence returns a session's latest evidence, newest first (insertion
// order, which is also time order since the daemon stamps it).
func (r Reader) Evidence(ctx context.Context, sessionID string, limit int) ([]api.StateEvidence, error) {
	rows, err := r.q.QueryContext(ctx,
		`SELECT at, layer, state, kind, detail FROM evidence WHERE session_id = ? ORDER BY id DESC LIMIT ?`,
		sessionID, limitOr(limit, 50))
	if err != nil {
		return nil, fmt.Errorf("list evidence: %w", err)
	}
	defer rows.Close()
	out := []api.StateEvidence{}
	for rows.Next() {
		var e api.StateEvidence
		var at int64
		var layer, st, kind string
		if err := rows.Scan(&at, &layer, &st, &kind, &e.Detail); err != nil {
			return nil, fmt.Errorf("list evidence: %w", err)
		}
		e.At = time.Unix(0, at).UTC()
		e.Layer, e.State, e.Kind = api.Layer(layer), api.State(st), api.BlockKind(kind)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- meta ----

// Meta reads a key from the meta table.
func (r Reader) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := r.q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read meta %s: %w", key, err)
	}
	return v, true, nil
}

// SetMeta writes a key to the meta table.
func (tx *Tx) SetMeta(ctx context.Context, key, value string) error {
	_, err := tx.q.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("write meta %s: %w", key, err)
	}
	return nil
}

// ---- retention ----

// PruneResult counts what Prune deleted.
type PruneResult struct {
	Requests, Events, Evidence int64
}

// prunableEvents are high-volume event types that only matter while recent.
// Lifecycle events (session started/exited, task updates) are kept so a
// session's timeline still shows how it began and ended.
var prunableEvents = []api.EventType{
	api.EventHook, api.EventPolicyDecision, api.EventSessionUpdated,
	api.EventRequestOpened, api.EventRequestClosed, api.EventPresenceChanged,
	api.EventDeviceChanged,
}

// Prune deletes closed requests, prunable events and evidence older than
// cutoff. The newest evidence per session and layer is kept so resolving a
// long-idle session's state still has something to go on. Sessions and
// tasks are never pruned.
func (tx *Tx) Prune(ctx context.Context, cutoff time.Time) (PruneResult, error) {
	var res PruneResult
	c := nanos(cutoff)

	args := []any{c}
	for _, s := range PendingStatuses {
		args = append(args, string(s))
	}
	n, err := tx.exec(ctx, `DELETE FROM requests WHERE closed_at IS NOT NULL AND closed_at < ?
		AND status NOT IN (`+placeholders(len(PendingStatuses))+`)`, args...)
	if err != nil {
		return res, fmt.Errorf("prune requests: %w", err)
	}
	res.Requests = n

	evArgs := []any{c}
	for _, t := range prunableEvents {
		evArgs = append(evArgs, string(t))
	}
	cond := `at < ? AND type IN (` + placeholders(len(prunableEvents)) + `)`
	var maxSeq sql.NullInt64
	if err := tx.q.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE `+cond, evArgs...).Scan(&maxSeq); err != nil {
		return res, fmt.Errorf("prune events: %w", err)
	}
	if n, err = tx.exec(ctx, `DELETE FROM events WHERE `+cond, evArgs...); err != nil {
		return res, fmt.Errorf("prune events: %w", err)
	}
	res.Events = n
	if maxSeq.Valid {
		prev, err := tx.PrunedSeq(ctx)
		if err != nil {
			return res, err
		}
		if maxSeq.Int64 > prev {
			if err := tx.SetMeta(ctx, prunedSeqKey, strconv.FormatInt(maxSeq.Int64, 10)); err != nil {
				return res, err
			}
		}
	}

	if n, err = tx.exec(ctx, `DELETE FROM evidence WHERE at < ?
		AND id NOT IN (SELECT MAX(id) FROM evidence GROUP BY session_id, layer)`, c); err != nil {
		return res, fmt.Errorf("prune evidence: %w", err)
	}
	res.Evidence = n
	return res, nil
}

func (tx *Tx) exec(ctx context.Context, q string, args ...any) (int64, error) {
	r, err := tx.q.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

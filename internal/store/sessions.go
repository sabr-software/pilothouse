package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
)

// Rows keep the full object as JSON in `data`; the other columns exist only
// to index and filter. That keeps the wire types the single definition of
// what a session or request is.

const sessionCols = `data`

// Session returns the session with exactly this id.
func (r Reader) Session(ctx context.Context, id string) (api.Session, error) {
	return scanSession(r.q.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = ?`, id))
}

// SessionByAgentID returns the most recently updated session the agent knows
// by agentSessionID.
func (r Reader) SessionByAgentID(ctx context.Context, agent api.AgentKind, agentSessionID string) (api.Session, error) {
	if agentSessionID == "" {
		return api.Session{}, ErrNotFound
	}
	return scanSession(r.q.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE agent = ? AND agent_session_id = ?
		 ORDER BY updated_at DESC LIMIT 1`, string(agent), agentSessionID))
}

// LiveSessionByPID returns a session of agent whose process is pid and that
// has not exited. Used when an agent's hooks carry no session id.
func (r Reader) LiveSessionByPID(ctx context.Context, agent api.AgentKind, pid int) (api.Session, error) {
	if pid <= 0 {
		return api.Session{}, ErrNotFound
	}
	return scanSession(r.q.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE agent = ? AND pid = ? AND exited_at IS NULL
		 ORDER BY updated_at DESC LIMIT 1`, string(agent), pid))
}

// LiveSessions returns every session that has not exited, newest first.
func (r Reader) LiveSessions(ctx context.Context) ([]api.Session, error) {
	return querySessions(ctx, r.q,
		`SELECT `+sessionCols+` FROM sessions WHERE exited_at IS NULL ORDER BY updated_at DESC`)
}

// Sessions returns live sessions plus those that exited at or after
// exitedSince (a zero time includes every session), newest first.
func (r Reader) Sessions(ctx context.Context, exitedSince time.Time) ([]api.Session, error) {
	if exitedSince.IsZero() {
		return querySessions(ctx, r.q, `SELECT `+sessionCols+` FROM sessions ORDER BY updated_at DESC`)
	}
	return querySessions(ctx, r.q,
		`SELECT `+sessionCols+` FROM sessions WHERE exited_at IS NULL OR exited_at >= ?
		 ORDER BY updated_at DESC`, nanos(exitedSince))
}

// ResolveSessionID expands an id or unique prefix ("s_ab12", or "ab12").
func (r Reader) ResolveSessionID(ctx context.Context, prefix string) (string, error) {
	return r.resolveID(ctx, "sessions", "s_", prefix)
}

// PutSession inserts or replaces a session.
func (tx *Tx) PutSession(ctx context.Context, s api.Session) error {
	if s.ID == "" {
		return errors.New("put session: empty id")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("put session %s: %w", s.ID, err)
	}
	_, err = tx.q.ExecContext(ctx, `
INSERT INTO sessions(id, agent, agent_session_id, pid, state, exited_at, updated_at, data)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
	agent = excluded.agent, agent_session_id = excluded.agent_session_id, pid = excluded.pid,
	state = excluded.state, exited_at = excluded.exited_at, updated_at = excluded.updated_at,
	data = excluded.data`,
		s.ID, string(s.Agent), s.AgentSessionID, s.PID, string(s.State),
		nullNanos(s.ExitedAt), nanos(s.UpdatedAt), string(b))
	if err != nil {
		return fmt.Errorf("put session %s: %w", s.ID, err)
	}
	return nil
}

func scanSession(row *sql.Row) (api.Session, error) {
	var data string
	if err := row.Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.Session{}, ErrNotFound
		}
		return api.Session{}, fmt.Errorf("read session: %w", err)
	}
	var s api.Session
	if err := json.Unmarshal([]byte(data), &s); err != nil {
		return api.Session{}, fmt.Errorf("decode session: %w", err)
	}
	return s, nil
}

func querySessions(ctx context.Context, q querier, query string, args ...any) ([]api.Session, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	out := []api.Session{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		var s api.Session
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			return nil, fmt.Errorf("decode session: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// resolveID finds the one id in table equal to or starting with prefix. A
// prefix without the kind marker ("ab12" for "s_ab12…") is retried with it,
// so people can type just the random part.
func (r Reader) resolveID(ctx context.Context, table, kind, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "", ErrNotFound
	}
	id, err := r.matchPrefix(ctx, table, prefix)
	if errors.Is(err, ErrNotFound) && !strings.HasPrefix(prefix, kind) {
		return r.matchPrefix(ctx, table, kind+prefix)
	}
	return id, err
}

// matchPrefix uses substr rather than LIKE because ids contain '_', which
// LIKE treats as a wildcard. table is always a constant from this package.
func (r Reader) matchPrefix(ctx context.Context, table, prefix string) (string, error) {
	var id string
	err := r.q.QueryRowContext(ctx, `SELECT id FROM `+table+` WHERE id = ?`, prefix).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve id: %w", err)
	}
	rows, err := r.q.QueryContext(ctx,
		`SELECT id FROM `+table+` WHERE substr(id, 1, ?) = ? LIMIT 2`, len(prefix), prefix)
	if err != nil {
		return "", fmt.Errorf("resolve id: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("resolve id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("resolve id: %w", err)
	}
	switch len(ids) {
	case 0:
		return "", ErrNotFound
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("%w: %q matches more than one", ErrAmbiguous, prefix)
	}
}

// nanos stores times as UTC Unix nanoseconds so range queries are integer
// comparisons.
func nanos(t time.Time) int64 { return t.UnixNano() }

func nullNanos(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

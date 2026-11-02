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

// prunedSeqKey records the highest seq ever deleted by retention. A client
// resuming from an older seq may have missed events and must resync.
const prunedSeqKey = "events_pruned_seq"

// Emit appends an event to the log inside the transaction. data is
// marshalled to JSON (json.RawMessage and []byte are stored verbatim). The
// event is published to subscribers only after the transaction commits.
func (tx *Tx) Emit(ctx context.Context, t api.EventType, sessionID, requestID string, data any) (api.Event, error) {
	raw, err := marshalData(data)
	if err != nil {
		return api.Event{}, fmt.Errorf("emit %s: %w", t, err)
	}
	at := tx.now
	var dataArg any
	if raw != nil {
		dataArg = string(raw)
	}
	res, err := tx.q.ExecContext(ctx,
		`INSERT INTO events(at, type, session_id, request_id, data) VALUES(?, ?, ?, ?, ?)`,
		nanos(at), string(t), sessionID, requestID, dataArg)
	if err != nil {
		return api.Event{}, fmt.Errorf("emit %s: %w", t, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return api.Event{}, fmt.Errorf("emit %s: %w", t, err)
	}
	ev := api.Event{Seq: seq, At: at, Type: t, SessionID: sessionID, RequestID: requestID, Data: raw}
	tx.events = append(tx.events, ev)
	return ev, nil
}

// Now is the transaction's clock reading, taken when it began. Using one
// instant per transaction keeps an event's At equal to the row it describes.
func (tx *Tx) Now() time.Time { return tx.now }

func marshalData(data any) (json.RawMessage, error) {
	switch v := data.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return v, nil
	case []byte:
		return json.RawMessage(v), nil
	default:
		return json.Marshal(v)
	}
}

// Events returns up to limit events with seq > after, oldest first.
func (r Reader) Events(ctx context.Context, after int64, limit int) ([]api.Event, error) {
	return queryEvents(ctx, r.q,
		`SELECT seq, at, type, session_id, request_id, data FROM events WHERE seq > ? ORDER BY seq LIMIT ?`,
		after, limitOr(limit, 1000))
}

// EventsRange returns up to limit events with after < seq <= upto, oldest first.
func (r Reader) EventsRange(ctx context.Context, after, upto int64, limit int) ([]api.Event, error) {
	return queryEvents(ctx, r.q,
		`SELECT seq, at, type, session_id, request_id, data FROM events WHERE seq > ? AND seq <= ? ORDER BY seq LIMIT ?`,
		after, upto, limitOr(limit, 1000))
}

// SessionEvents returns a session's latest events, newest first.
func (r Reader) SessionEvents(ctx context.Context, sessionID string, limit int) ([]api.Event, error) {
	return queryEvents(ctx, r.q,
		`SELECT seq, at, type, session_id, request_id, data FROM events WHERE session_id = ? ORDER BY seq DESC LIMIT ?`,
		sessionID, limitOr(limit, 100))
}

// MaxSeq is the seq of the newest event, 0 if the log is empty. It reads
// sqlite_sequence so it stays correct even when the newest rows were pruned.
func (r Reader) MaxSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	err := r.q.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'events'`).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("max seq: %w", err)
	}
	return seq.Int64, nil
}

// PrunedSeq is the highest seq deleted by retention (0 if none).
func (r Reader) PrunedSeq(ctx context.Context) (int64, error) {
	v, ok, err := r.Meta(ctx, prunedSeqKey)
	if err != nil || !ok {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad %s %q", prunedSeqKey, v)
	}
	return n, nil
}

func queryEvents(ctx context.Context, q querier, query string, args ...any) ([]api.Event, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	out := []api.Event{}
	for rows.Next() {
		var (
			ev   api.Event
			at   int64
			typ  string
			data sql.NullString
		)
		if err := rows.Scan(&ev.Seq, &at, &typ, &ev.SessionID, &ev.RequestID, &data); err != nil {
			return nil, fmt.Errorf("list events: %w", err)
		}
		ev.At = time.Unix(0, at).UTC()
		ev.Type = api.EventType(typ)
		if data.Valid {
			ev.Data = json.RawMessage(data.String)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func limitOr(limit, def int) int {
	if limit <= 0 || limit > def*10 {
		return def
	}
	return limit
}

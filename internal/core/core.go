// Package core declares the daemon's internal service boundary. The daemon
// (internal/daemon) implements Core; the web server, notifier and other
// subsystems depend only on these interfaces.
package core

import (
	"context"
	"errors"

	"github.com/sabr-software/pilothouse/internal/api"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyClosed = errors.New("request already closed")
	ErrForbidden     = errors.New("forbidden")
)

// Subscription is a live tail of the event log.
type Subscription struct {
	// C delivers events in Seq order. It is closed when the subscription ends.
	C <-chan api.Event
	// Resync is true when the requested `after` is older than retained
	// history (or the subscriber fell behind); the client must reload a
	// Snapshot. C is closed immediately in that case.
	Resync bool
	Close  func()
}

// Core is the daemon's state machine, as seen by other subsystems.
type Core interface {
	Snapshot(ctx context.Context) (api.Snapshot, error)
	Subscribe(ctx context.Context, after int64) (*Subscription, error)

	Session(ctx context.Context, id string) (api.Session, error)
	SessionEvents(ctx context.Context, sessionID string, limit int) ([]api.Event, error)
	MarkSeen(ctx context.Context, sessionID string) error

	Request(ctx context.Context, id string) (api.Request, error)
	// Decide closes an open request. The first decision wins; later ones
	// get ErrAlreadyClosed. d.By must identify the decider.
	Decide(ctx context.Context, d api.Decision) (api.Request, error)

	Presence() api.Presence
	SetPresenceMode(ctx context.Context, m api.PresenceMode) error

	// Emit appends an event to the log and fans it out to subscribers.
	Emit(ctx context.Context, t api.EventType, sessionID, requestID string, data any) (api.Event, error)
}

// NotificationKind says why a notification is sent.
type NotificationKind string

const (
	NotifyRequest NotificationKind = "request" // a permission request is held for devices
	NotifyBlocked NotificationKind = "blocked" // a native dialog has waited longer than notify_after
	NotifyWaiting NotificationKind = "waiting" // agent finished its turn
	NotifyTest    NotificationKind = "test"
)

// Notification is a message to a person.
type Notification struct {
	Kind    NotificationKind `json:"kind"`
	Title   string           `json:"title"`
	Body    string           `json:"body"`
	URL     string           `json:"url,omitempty"` // deep link into the web app, if reachable
	Request *api.Request     `json:"request,omitempty"`
	Session *api.Session     `json:"session,omitempty"`
	// Remote is true when the person is away: send to devices (push, ntfy,
	// webhooks). False: desktop notification only.
	Remote bool `json:"remote"`
	// Linked is true for a notification forwarded from a linked machine
	// (`pilothouse link`). Channels then offer no one-tap answers: only a
	// paired device can answer, and the linked machine checks its
	// signature itself.
	Linked bool `json:"linked,omitempty"`
}

// Notifier delivers notifications. Implementations must not block for
// long: deliver asynchronously and log failures.
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}

// NopNotifier drops every notification.
type NopNotifier struct{}

func (NopNotifier) Notify(context.Context, Notification) error { return nil }

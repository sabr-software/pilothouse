package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/store"
)

// presenceModeKey persists the here/away/auto override across restarts.
const presenceModeKey = "presence_mode"

// presenceState is the current routing input. update serializes changes
// (and their events); mu guards reads so hooks never wait on a write.
type presenceState struct {
	update sync.Mutex
	mu     sync.RWMutex
	cur    api.Presence
}

// Presence returns the current presence.
func (d *Daemon) Presence() api.Presence {
	d.pres.mu.RLock()
	defer d.pres.mu.RUnlock()
	return d.pres.cur
}

// SetPresenceMode sets the manual override (auto, here, away), persists it
// and applies it at once.
func (d *Daemon) SetPresenceMode(ctx context.Context, m api.PresenceMode) error {
	switch m {
	case api.PresenceAuto, api.PresenceForceHere, api.PresenceForceAway:
	default:
		return badRequest("presence mode must be auto, here or away, not %q", m)
	}
	return d.updatePresence(ctx, &m)
}

func (d *Daemon) initPresence(ctx context.Context) error {
	mode := api.PresenceAuto
	v, ok, err := d.db.Meta(ctx, presenceModeKey)
	if err != nil {
		return err
	}
	if ok {
		mode = api.PresenceMode(v)
	}
	p := d.measurePresence(mode)
	p.ChangedAt = d.now()
	d.pres.mu.Lock()
	d.pres.cur = p
	d.pres.mu.Unlock()
	return nil
}

// measurePresence computes presence for mode from the OS idle detector.
// Unknown idle time counts as present: routing to the terminal is the safe
// default, since the agent's own dialog is always there.
func (d *Daemon) measurePresence(mode api.PresenceMode) api.Presence {
	p := api.Presence{Mode: mode, State: api.PresencePresent}
	switch mode {
	case api.PresenceForceHere:
		p.Source = "manual"
	case api.PresenceForceAway:
		p.State, p.Source = api.PresenceAway, "manual"
	default:
		p.Mode = api.PresenceAuto
		idle, ok := d.detector.Idle()
		if !ok {
			p.Source = "unknown"
			break
		}
		p.Source = "os_idle"
		p.IdleSecs = int(idle.Seconds())
		if idle >= d.cfg.Presence.IdleThreshold.Duration {
			p.State = api.PresenceAway
		}
	}
	return p
}

// updatePresence re-measures presence (with a new mode, if given), emits
// presence.changed when state, mode or source changed, and releases held
// requests when the person comes back.
func (d *Daemon) updatePresence(ctx context.Context, newMode *api.PresenceMode) error {
	d.pres.update.Lock()
	defer d.pres.update.Unlock()

	prev := d.Presence()
	mode := prev.Mode
	if newMode != nil {
		mode = *newMode
	}
	next := d.measurePresence(mode)
	next.ChangedAt = prev.ChangedAt
	changed := next.State != prev.State || next.Mode != prev.Mode || next.Source != prev.Source
	if changed {
		next.ChangedAt = d.now()
		err := d.db.Write(ctx, func(tx *store.Tx) error {
			if next.Mode != prev.Mode {
				if err := tx.SetMeta(ctx, presenceModeKey, string(next.Mode)); err != nil {
					return err
				}
			}
			_, err := tx.Emit(ctx, api.EventPresenceChanged, "", "", next)
			return err
		})
		if err != nil {
			return fmt.Errorf("record presence: %w", err)
		}
		d.log.Info("presence changed", "state", next.State, "mode", next.Mode, "source", next.Source)
	}
	d.pres.mu.Lock()
	d.pres.cur = next
	d.pres.mu.Unlock()

	if prev.State == api.PresenceAway && next.State == api.PresencePresent {
		d.releaseHeld(ctx, "person is back at the desk")
	}
	return nil
}

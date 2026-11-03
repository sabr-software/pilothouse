package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/desktop"
	"github.com/sabr-software/pilothouse/internal/store"
)

// herdrNamesEvery is how often session names are read from herdr.
const herdrNamesEvery = 10 * time.Second

// syncHerdrNames names each live session that runs in a herdr tab the way
// herdr does ("workspace · tab"), matched by the agent's own session id,
// so the app, the inbox and notifications call it what the person sees
// on the desktop. Task sessions keep their task's name. herdr not
// installed or not running changes nothing.
func (d *Daemon) syncHerdrNames(ctx context.Context) {
	names, err := d.desktop().HerdrNames(ctx)
	if err != nil {
		if !errors.Is(err, desktop.ErrUnavailable) && ctx.Err() == nil {
			d.log.Debug("herdr names", "err", err)
		}
		return
	}
	if len(names) == 0 {
		return
	}
	live, err := d.db.LiveSessions(ctx)
	if err != nil {
		return
	}
	rename := map[string]string{}
	for _, s := range live {
		if n := names[s.AgentSessionID]; s.AgentSessionID != "" && s.TaskID == "" && n != "" && n != s.Title {
			rename[s.ID] = n
		}
	}
	if len(rename) == 0 {
		return
	}
	err = d.db.Write(ctx, func(tx *store.Tx) error {
		for id, n := range rename {
			s, err := tx.Session(ctx, id)
			if err != nil || s.State == api.StateExited || s.Title == n {
				continue
			}
			// A new name isn't activity: UpdatedAt stays.
			s.Title = n
			if err := tx.PutSession(ctx, s); err != nil {
				return err
			}
			if _, err := tx.Emit(ctx, api.EventSessionUpdated, s.ID, "", s); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && ctx.Err() == nil {
		d.log.Warn("herdr names", "err", err)
	}
}

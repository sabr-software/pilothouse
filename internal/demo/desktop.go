package demo

import (
	"context"
	"fmt"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
)

var _ core.Desktop = (*Core)(nil)

// DesktopInfo pretends herdr is running on the demo machine.
func (c *Core) DesktopInfo(context.Context) api.DesktopInfo {
	return api.DesktopInfo{Target: api.DesktopHerdr, Label: "herdr", Machine: c.Machine}
}

// OpenOnDesktop pretends to add a herdr tab; a second open of the same
// session reuses it, as the real one does.
func (c *Core) OpenOnDesktop(_ context.Context, id, _ string) (api.DesktopOpened, error) {
	s, ok := c.session(id)
	switch {
	case !ok:
		return api.DesktopOpened{}, core.ErrNotFound
	case s.State == api.StateExited:
		return api.DesktopOpened{}, core.Invalid("this session has ended")
	case s.Mode != api.ModeWrapped || !s.Caps.Watch:
		return api.DesktopOpened{}, fmt.Errorf("%w: only sessions started with `pilothouse run` or as a task have a terminal Pilothouse can open — this one runs in a terminal of its own", core.ErrUnsupported)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.desktopTabs == nil {
		c.desktopTabs = map[string]bool{}
	}
	reused := c.desktopTabs[id]
	c.desktopTabs[id] = true
	return api.DesktopOpened{Target: api.DesktopHerdr, Label: "herdr", Where: s.Title, Reused: reused, Machine: c.Machine}, nil
}

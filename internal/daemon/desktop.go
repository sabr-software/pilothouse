package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/core"
	"github.com/sabr-software/pilothouse/internal/desktop"
	"github.com/sabr-software/pilothouse/internal/paths"
	"github.com/sabr-software/pilothouse/internal/wrap"
)

// Opening sessions on the desktop (desktop.Opener): the web app's "Open
// in herdr / Terminal" and, with desktop.open_tasks, every task a device
// starts.

var _ core.Desktop = (*Daemon)(nil)

// A task's session may take a moment to register after its agent starts.
const (
	taskOpenTries = 20
	taskOpenPause = 500 * time.Millisecond
)

type deskState struct {
	once   sync.Once
	opener *desktop.Opener
}

func (d *Daemon) desktop() *desktop.Opener {
	d.desk.once.Do(func() {
		if d.opts.Desktop != nil {
			d.desk.opener = d.opts.Desktop
			return
		}
		c := d.cfg.Desktop
		d.desk.opener = desktop.New(c.Open, c.TerminalApp, c.HerdrSession, pilothouseBinary(), filepath.Join(paths.Run(), "desktop"))
		// `pilothouse attach` in the new terminal must find this daemon.
		for _, k := range []string{"PILOTHOUSE_HOME", "PILOTHOUSE_RUN_DIR"} {
			if v := os.Getenv(k); v != "" {
				d.desk.opener.Env = append(d.desk.opener.Env, k+"="+v)
			}
		}
	})
	return d.desk.opener
}

// pilothouseBinary is the program typed into the new terminal. The PATH
// entry (Homebrew's symlink) is preferred when it is this program: the
// resolved path changes with every upgrade.
func pilothouseBinary() string {
	self, err := os.Executable()
	if err != nil {
		self = ""
	}
	if p, err := exec.LookPath("pilothouse"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			a, errA := filepath.EvalSymlinks(abs)
			b, errB := filepath.EvalSymlinks(self)
			if self == "" || (errA == nil && errB == nil && a == b) {
				return abs
			}
		}
	}
	return self
}

// DesktopInfo says where a session would open on this computer.
func (d *Daemon) DesktopInfo(ctx context.Context) api.DesktopInfo {
	info := d.desktop().Info(ctx)
	if info.Target != "" {
		info.Machine = d.machine
	}
	return info
}

// OpenOnDesktop opens a wrapped session on this computer and brings it
// to the front.
func (d *Daemon) OpenOnDesktop(ctx context.Context, id, by string) (api.DesktopOpened, error) {
	s, err := d.Session(ctx, id)
	if err != nil {
		return api.DesktopOpened{}, err
	}
	if err := desktopReady(ctx, s); err != nil {
		return api.DesktopOpened{}, err
	}
	res, err := d.desktop().Open(ctx, d.desktopSpec(ctx, s, true))
	if err != nil {
		return res, desktopErr(err)
	}
	res.Machine = d.machine
	d.log.Info("session opened on the desktop", "session", s.ID, "in", res.Label, "where", res.Where, "reused", res.Reused, "by", by)
	return res, nil
}

// desktopReady refuses sessions without a live terminal to attach to.
func desktopReady(ctx context.Context, s api.Session) error {
	switch {
	case s.ExitedAt != nil || s.State == api.StateExited:
		return core.Invalid("this session has ended")
	case s.Mode != api.ModeWrapped || !s.Caps.Watch:
		return fmt.Errorf("%w: only sessions started with `pilothouse run` or as a task have a terminal Pilothouse can open — this one runs in a terminal of its own", core.ErrUnsupported)
	case !wrap.Alive(ctx, s.ID):
		return core.Invalid("this session's terminal has closed")
	}
	return nil
}

func (d *Daemon) desktopSpec(ctx context.Context, s api.Session, focus bool) desktop.Spec {
	spec := desktop.Spec{SessionID: s.ID, Title: s.Title, Cwd: s.Cwd, Repo: s.RepoRoot, Focus: focus}
	if s.TaskID != "" {
		if t, err := d.findTask(ctx, s.TaskID); err == nil {
			spec.Repo = t.Repo // the main checkout, which a herdr workspace is likely to be
			if t.Title != "" {
				spec.Title = t.Title
			}
		}
	}
	return spec
}

// desktopErr makes an Opener error safe and useful to show.
func desktopErr(err error) error {
	if errors.Is(err, desktop.ErrUnavailable) {
		return fmt.Errorf("%w: %s", core.ErrUnsupported, strings.TrimPrefix(err.Error(), desktop.ErrUnavailable.Error()+": "))
	}
	return core.Invalid("couldn't open it on the computer: %v", err)
}

// openTaskOnDesktop opens a task's session in the background as it
// starts, when desktop.open_tasks is on. Failures are logged only: the
// task runs either way.
func (d *Daemon) openTaskOnDesktop(t api.Task) {
	if !d.cfg.Desktop.OpenTasks || t.SessionID == "" {
		return
	}
	d.goBackground(func() {
		ctx := d.ctx
		// The holder registers the session as it starts: wait for it.
		var s api.Session
		var err error
		for i := 0; i < taskOpenTries; i++ {
			if s, err = d.Session(ctx, t.SessionID); err == nil {
				if err = desktopReady(ctx, s); err == nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(taskOpenPause):
			}
		}
		if err == nil {
			_, err = d.desktop().Open(ctx, d.desktopSpec(ctx, s, false))
		}
		if err != nil {
			d.log.Info("task not opened on the desktop", "task", t.ID, "err", err)
			return
		}
		d.log.Info("task opened on the desktop", "task", t.ID, "session", t.SessionID)
	})
}

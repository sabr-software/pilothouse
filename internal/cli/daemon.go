package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
	"github.com/sabr-software/pilothouse/internal/config"
	"github.com/sabr-software/pilothouse/internal/daemon"
	"github.com/sabr-software/pilothouse/internal/paths"
)

const (
	logMaxBytes    = 10 << 20 // rotate daemon.log at 10 MB…
	logKeep        = 3        // …keeping daemon.log.1 … .3
	startTimeout   = 5 * time.Second
	stopTimeout    = 10 * time.Second
	exitNotRunning = 3 // like `systemctl status`: "program is not running"
)

func newDaemonCmd() *cobra.Command {
	var logFile string
	var debug bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the Pilothouse daemon in the foreground",
		Long: `Run the Pilothouse daemon in the foreground. Most people use ` + "`pilothouse start`" + `
or the background service (` + "`pilothouse service install`" + `) instead.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var out io.Writer = os.Stderr
			if logFile != "" {
				rf, err := openRotatingFile(logFile, logMaxBytes, logKeep)
				if err != nil {
					return err
				}
				defer rf.Close()
				out = rf
			}
			level := slog.LevelInfo
			if debug {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
			ctx, stop := signal.NotifyContext(cmdContext(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err := daemon.Run(ctx, daemon.Options{Log: log})
			if err != nil {
				log.Error("daemon exited", "err", err)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&logFile, "log-file", "", "write logs to this file (rotated at 10 MB, 3 kept) instead of stderr")
	cmd.Flags().BoolVar(&debug, "debug", false, "log at debug level")
	return cmd
}

func newStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmdContext(cmd)
			c := client.New()
			if h, err := quickHealth(ctx, c); err == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "pilothouse daemon is already running (pid %d)\n", h.PID)
				return nil
			}
			pid, err := spawnDaemon()
			if err != nil {
				return err
			}
			h, err := waitHealthy(ctx, c, startTimeout)
			if err != nil {
				return fmt.Errorf("the daemon (pid %d) did not come up within %s: see %s", pid, startTimeout, daemonLogPath())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pilothouse daemon started (pid %d)\n", h.PID)
			if h.WebURL != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "web app: %s\n", h.WebURL)
			} else if cfg, err := config.Load(); err == nil && cfg.Web.Addr != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "! The web app didn't start, so no phone or browser can reach this daemon. Its port is probably in use by another Pilothouse: see %s\n", daemonLogPath())
			}
			return nil
		},
	}
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background daemon",
		Long: `Stop the daemon. Requests it is holding are handed back to the agents'
own dialogs first. Agents started with ` + "`pilothouse run`" + ` keep running.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmdContext(cmd)
			c := client.New()
			err := c.Shutdown(ctx)
			if errors.Is(err, client.ErrDaemonDown) {
				fmt.Fprintln(cmd.OutOrStdout(), "pilothouse daemon is not running")
				return nil
			}
			if err != nil {
				return fmt.Errorf("stop daemon: %w", err)
			}
			if err := waitGone(ctx, c, stopTimeout); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "pilothouse daemon stopped")
			return nil
		},
	}
}

// daemonStatus is `pilothouse status --json`.
type daemonStatus struct {
	Running      bool          `json:"running"`
	Health       *api.Health   `json:"health,omitempty"`
	Sessions     int           `json:"sessions"`
	OpenRequests int           `json:"open_requests"`
	Presence     *api.Presence `json:"presence,omitempty"`
}

func newStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmdContext(cmd)
			st, err := collectStatus(ctx, client.New())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(st); err != nil {
					return err
				}
			} else {
				printStatus(out, st)
			}
			if !st.Running {
				return ExitError{Code: exitNotRunning}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")
	return cmd
}

func collectStatus(ctx context.Context, c *client.Client) (daemonStatus, error) {
	h, err := quickHealth(ctx, c)
	if errors.Is(err, client.ErrDaemonDown) {
		return daemonStatus{}, nil
	}
	if err != nil {
		return daemonStatus{}, fmt.Errorf("daemon health: %w", err)
	}
	st := daemonStatus{Running: true, Health: &h}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return st, fmt.Errorf("daemon snapshot: %w", err)
	}
	for _, s := range snap.Sessions {
		if s.State != api.StateExited {
			st.Sessions++
		}
	}
	for _, r := range snap.Requests {
		if r.Status == api.RequestOpen || r.Status == api.RequestSteppedAside {
			st.OpenRequests++
		}
	}
	st.Presence = &snap.Presence
	return st, nil
}

func printStatus(w io.Writer, st daemonStatus) {
	if !st.Running {
		fmt.Fprintln(w, "daemon:   not running — start it with `pilothouse start`")
		return
	}
	h := st.Health
	fmt.Fprintf(w, "daemon:   running (pid %d, version %s)\n", h.PID, h.Version)
	fmt.Fprintf(w, "machine:  %s\n", h.Machine)
	fmt.Fprintf(w, "sessions: %d active, %d request(s) waiting\n", st.Sessions, st.OpenRequests)
	if p := st.Presence; p != nil {
		mode := ""
		if p.Mode != api.PresenceAuto {
			mode = fmt.Sprintf(", forced with `pilothouse %s`", p.Mode)
		}
		fmt.Fprintf(w, "presence: %s (%s%s)\n", p.State, p.Source, mode)
	}
	if h.WebURL != "" {
		fmt.Fprintf(w, "web app:  %s\n", h.WebURL)
	} else {
		fmt.Fprintln(w, "web app:  not listening (see the daemon log)")
	}
	fmt.Fprintf(w, "socket:   %s\n", paths.Socket())
}

// spawnDaemon re-executes this binary as `pilothouse daemon` in its own
// session, detached from the terminal, so closing the terminal doesn't
// stop it.
func spawnDaemon() (int, error) {
	if err := paths.Ensure(); err != nil {
		return 0, fmt.Errorf("create %s: %w", paths.Home(), err)
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find the pilothouse binary: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "daemon", "--log-file", daemonLogPath())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// Reap it if it dies while we wait, so it isn't a zombie; otherwise we
	// exit first and init adopts it.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

func daemonLogPath() string { return filepath.Join(paths.Logs(), "daemon.log") }

func quickHealth(ctx context.Context, c *client.Client) (api.Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.Health(ctx)
}

func waitHealthy(ctx context.Context, c *client.Client, timeout time.Duration) (api.Health, error) {
	deadline := time.Now().Add(timeout)
	for {
		h, err := quickHealth(ctx, c)
		if err == nil {
			return h, nil
		}
		if time.Now().After(deadline) {
			return api.Health{}, err
		}
		select {
		case <-ctx.Done():
			return api.Health{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// waitGone waits until the daemon stops answering and its socket is gone.
func waitGone(ctx context.Context, c *client.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, statErr := os.Stat(paths.Socket())
		if _, err := quickHealth(ctx, c); errors.Is(err, client.ErrDaemonDown) && os.IsNotExist(statErr) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the daemon is still running after %s: see %s", timeout, daemonLogPath())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func cmdContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// rotatingFile is an append-only log file that rotates itself at maxBytes,
// keeping `keep` old files (name.1 newest … name.N oldest). The daemon is
// its only writer, so rotation needs no cross-process coordination.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
	size     int64
}

func openRotatingFile(path string, maxBytes int64, keep int) (*rotatingFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	rf := &rotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := rf.open(); err != nil {
		return nil, err
	}
	return rf, nil
}

func (rf *rotatingFile) open() error {
	f, err := os.OpenFile(rf.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", rf.path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat log %s: %w", rf.path, err)
	}
	rf.f, rf.size = f, fi.Size()
	return nil
}

func (rf *rotatingFile) Write(p []byte) (int, error) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.f == nil {
		return 0, os.ErrClosed
	}
	if rf.size > 0 && rf.size+int64(len(p)) > rf.maxBytes {
		if err := rf.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := rf.f.Write(p)
	rf.size += int64(n)
	return n, err
}

func (rf *rotatingFile) rotate() error {
	if err := rf.f.Close(); err != nil {
		return err
	}
	rf.f = nil
	for i := rf.keep - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", rf.path, i), fmt.Sprintf("%s.%d", rf.path, i+1))
	}
	if rf.keep > 0 {
		_ = os.Rename(rf.path, rf.path+".1")
	} else {
		_ = os.Remove(rf.path)
	}
	return rf.open()
}

func (rf *rotatingFile) Close() error {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if rf.f == nil {
		return nil
	}
	err := rf.f.Close()
	rf.f = nil
	return err
}

package wrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sabr-software/pilothouse/internal/paths"
)

// SpawnOptions describes a holder to start in the background.
type SpawnOptions struct {
	Exe       string   // pilothouse binary; default: os.Executable()
	SessionID string   // default: NewSessionID()
	Name      string   // default: basename of Dir
	TaskID    string   //
	Argv      []string // agent command; required
	Dir       string   // working directory; default: current
	Env       []string // holder (and so agent) environment; nil: inherit
	Cols      uint16   // initial PTY size; default 80x24
	Rows      uint16
	Timeout   time.Duration // how long to wait for the socket; default 5s
}

// HolderLogPath is where a holder writes its log.
func HolderLogPath(sessionID string) string {
	return filepath.Join(paths.Logs(), "holder-"+sessionID+".log")
}

// holderLogRetention: holder logs are kept for post-mortems, then pruned
// whenever a new holder is spawned.
const holderLogRetention = 7 * 24 * time.Hour

// Spawn starts `pilothouse __holder` detached from this process (own session,
// no terminal) and returns the session id once the holder's socket
// answers. The holder is never a child of the daemon or a service manager,
// so restarting either never kills an agent.
func Spawn(ctx context.Context, o SpawnOptions) (string, error) {
	if len(o.Argv) == 0 {
		return "", errors.New("wrap: no command to run")
	}
	if o.Exe == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("wrap: find pilothouse binary: %w", err)
		}
		o.Exe = exe
	}
	if o.SessionID == "" {
		o.SessionID = NewSessionID()
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Second
	}
	if err := paths.Ensure(); err != nil {
		return "", err
	}
	pruneHolderLogs()

	args := []string{"__holder", "--id", o.SessionID}
	if o.Name != "" {
		args = append(args, "--name", o.Name)
	}
	if o.TaskID != "" {
		args = append(args, "--task", o.TaskID)
	}
	if o.Cols > 0 && o.Rows > 0 {
		args = append(args, "--cols", strconv.Itoa(int(o.Cols)), "--rows", strconv.Itoa(int(o.Rows)))
	}
	args = append(append(args, "--"), o.Argv...)

	logPath := HolderLogPath(o.SessionID)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("wrap: open holder log: %w", err)
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return "", err
	}
	defer devnull.Close()

	cmd := exec.Command(o.Exe, args...)
	cmd.Dir = o.Dir
	cmd.Env = o.Env
	// No terminal: stdin/stdout are /dev/null. stderr goes to the holder
	// log so a Go runtime panic is recorded instead of lost.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("wrap: start holder: %w", err)
	}
	exited := make(chan struct{})
	// Reap the holder if it exits while we are still running (otherwise
	// it would linger as a zombie until this process ends).
	go func() { _ = cmd.Wait(); close(exited) }()

	sock := paths.HolderSocket(o.SessionID)
	deadline := time.NewTimer(o.Timeout)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := Probe(pctx, sock)
		cancel()
		if err == nil {
			return o.SessionID, nil
		}
		select {
		case <-exited:
			return "", fmt.Errorf("wrap: holder exited during startup%s", logTail(logPath))
		case <-deadline.C:
			_ = cmd.Process.Kill()
			return "", fmt.Errorf("wrap: holder did not come up within %s%s", o.Timeout, logTail(logPath))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return "", ctx.Err()
		case <-tick.C:
		}
	}
}

// logTail returns the last lines of a holder log, formatted for an error.
func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return " (log: " + path + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return ":\n  " + strings.Join(lines, "\n  ") + "\n(log: " + path + ")"
}

func pruneHolderLogs() {
	ents, err := os.ReadDir(paths.Logs())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-holderLogRetention)
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "holder-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(paths.Logs(), e.Name()))
		}
	}
}

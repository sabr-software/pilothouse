// Package proc inspects processes: ancestry for the hook shim and the
// self-approval check, liveness for layer L0, Unix-socket peer PIDs.
//
// The OS-specific halves live in proc_darwin.go (sysctl) and proc_linux.go
// (/proc). Both are read-only and never signal anything except signal 0.
package proc

import (
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

// maxArgs caps Proc.Args: ancestry travels in every hook envelope, and a
// long command line (a prompt passed as argv) would bloat each one.
const maxArgs = 1024

// ErrUnsupported is returned on platforms Pilothouse does not support.
var ErrUnsupported = errors.New("process inspection is not supported on this platform")

// Ancestors returns pid's ancestors, nearest first (pid itself excluded),
// up to max entries, stopping at PID 1 (which is not included: init/launchd
// says nothing about who started a process).
func Ancestors(pid, max int) ([]adapters.Proc, error) {
	if max <= 0 {
		return nil, nil
	}
	self, err := Info(pid)
	if err != nil {
		return nil, err
	}
	var out []adapters.Proc
	seen := map[int]bool{pid: true}
	next := self.PPID
	for len(out) < max && next > 1 && !seen[next] {
		seen[next] = true
		p, err := Info(next)
		if err != nil {
			// The parent can exit between two reads; a partial chain is
			// still useful to the caller.
			break
		}
		out = append(out, p)
		next = p.PPID
	}
	return out, nil
}

// Info returns one process.
func Info(pid int) (adapters.Proc, error) {
	if pid <= 0 {
		return adapters.Proc{}, fmt.Errorf("invalid pid %d", pid)
	}
	return info(pid)
}

// Alive reports whether pid exists (signal 0). EPERM means it exists but
// belongs to another user.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// PeerPID returns the PID of the process on the other end of a Unix socket.
func PeerPID(c net.Conn) (int, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return 0, fmt.Errorf("peer pid: %T is not a socket", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var perr error
	if err := raw.Control(func(fd uintptr) { pid, perr = peerPID(int(fd)) }); err != nil {
		return 0, err
	}
	if perr != nil {
		return 0, fmt.Errorf("peer pid: %w", perr)
	}
	return pid, nil
}

// commFromArgv picks the name a person (and ProcessNames) would recognize:
// argv[0]'s base name reflects process titles set by Node-based agents and
// the name an agent was invoked by, which the kernel's truncated comm does
// not. Login shells prefix argv[0] with "-".
func commFromArgv(argv0, kernelComm string) string {
	name := strings.TrimPrefix(filepath.Base(argv0), "-")
	if argv0 == "" || name == "" || name == "." || name == "/" {
		return kernelComm
	}
	return name
}

// joinArgs renders argv for display, capped at maxArgs bytes.
func joinArgs(argv []string) string {
	s := strings.Join(argv, " ")
	if len(s) <= maxArgs {
		return s
	}
	cut := maxArgs
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// utf8Start reports whether b can begin a UTF-8 sequence, so truncation
// never splits a rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

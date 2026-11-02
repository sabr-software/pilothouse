//go:build linux

package proc

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

// info reads /proc/<pid>/stat for the parent and kernel name and
// /proc/<pid>/cmdline for argv (empty for kernel threads and zombies).
func info(pid int) (adapters.Proc, error) {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return adapters.Proc{}, fmt.Errorf("process %d: %w", pid, err)
	}
	comm, ppid, err := parseStat(stat)
	if err != nil {
		return adapters.Proc{}, fmt.Errorf("process %d: %w", pid, err)
	}
	p := adapters.Proc{PID: pid, PPID: ppid, Comm: comm}
	if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil && len(raw) > 0 {
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		p.Comm = commFromArgv(argv[0], comm)
		p.Args = joinArgs(argv)
	}
	return p, nil
}

// parseStat extracts comm and ppid from /proc/<pid>/stat. comm is wrapped
// in parentheses and may itself contain spaces or ")", so the last ")"
// ends it.
func parseStat(b []byte) (comm string, ppid int, err error) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, fmt.Errorf("malformed stat")
	}
	comm = string(b[open+1 : end])
	fields := strings.Fields(string(b[end+1:]))
	// fields[0] is the state, fields[1] the parent PID.
	if len(fields) < 2 {
		return "", 0, fmt.Errorf("malformed stat")
	}
	ppid, err = strconv.Atoi(fields[1])
	return comm, ppid, err
}

func peerPID(fd int) (int, error) {
	cred, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return int(cred.Pid), nil
}

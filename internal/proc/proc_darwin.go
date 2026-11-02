//go:build darwin

package proc

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

// info reads kern.proc.pid for the parent and kernel name, and
// kern.procargs2 for argv. procargs2 fails for other users' processes
// (unless root); the kernel name is used then.
func info(pid int) (adapters.Proc, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return adapters.Proc{}, fmt.Errorf("process %d: %w", pid, err)
	}
	// sysctl returns a zeroed record instead of an error for a PID that
	// does not exist.
	if int(kp.Proc.P_pid) != pid {
		return adapters.Proc{}, fmt.Errorf("process %d: %w", pid, unix.ESRCH)
	}
	comm := cString(kp.Proc.P_comm[:])
	p := adapters.Proc{PID: pid, PPID: int(kp.Eproc.Ppid), Comm: comm}
	if argv, err := procArgs(pid); err == nil && len(argv) > 0 {
		p.Comm = commFromArgv(argv[0], comm)
		p.Args = joinArgs(argv)
	}
	return p, nil
}

// procArgs parses kern.procargs2: a native-endian int32 argc, the exec
// path, NUL padding, then argc NUL-terminated arguments (then the
// environment, which is ignored).
func procArgs(pid int) ([]string, error) {
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, err
	}
	return parseProcArgs2(buf)
}

func parseProcArgs2(buf []byte) ([]string, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("procargs2: short buffer")
	}
	argc := int(binary.NativeEndian.Uint32(buf[:4]))
	rest := buf[4:]
	// Skip the exec path and the NULs that pad it.
	i := bytes.IndexByte(rest, 0)
	if i < 0 {
		return nil, fmt.Errorf("procargs2: no exec path")
	}
	rest = rest[i:]
	for len(rest) > 0 && rest[0] == 0 {
		rest = rest[1:]
	}
	argv := make([]string, 0, min(argc, 64))
	for len(argv) < argc && len(rest) > 0 {
		j := bytes.IndexByte(rest, 0)
		if j < 0 {
			argv = append(argv, string(rest))
			break
		}
		argv = append(argv, string(rest[:j]))
		rest = rest[j+1:]
	}
	return argv, nil
}

func peerPID(fd int) (int, error) {
	return unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

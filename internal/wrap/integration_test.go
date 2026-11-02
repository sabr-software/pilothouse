package wrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/paths"
)

// These tests drive the real pilothouse binary: `run -d` spawns a detached
// holder process, and `run`/`attach` run inside a PTY like a user's
// terminal.

func runDetached(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"run", "-d"}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pilothouse run -d: %v\n%s", err, stderr.String())
	}
	sid := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(sid, "s_") || !strings.Contains(stderr.String(), "pilothouse attach "+sid) {
		t.Fatalf("run -d printed %q / %q", sid, stderr.String())
	}
	return sid
}

func TestBinaryDetachedHolderNoDaemon(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	sid := runDetached(t, bin, "-n", "fake", "--", "/bin/sh", "-c", fakeAgent)
	sock := paths.HolderSocket(sid)

	info, err := Probe(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "fake" || info.HolderPID == os.Getpid() || info.PID == 0 {
		t.Fatalf("probe %+v", info)
	}
	if err := syscall.Kill(info.HolderPID, 0); err != nil {
		t.Fatalf("holder process: %v", err)
	}

	// Attach, read, disconnect abruptly: the holder must survive.
	a := attachSession(t, sock, true)
	a.waitOutput("ready")
	a.c.Close()
	<-a.done
	time.Sleep(100 * time.Millisecond)
	if _, err := Probe(context.Background(), sock); err != nil {
		t.Fatalf("holder died with its client: %v", err)
	}

	// Reattach, drive the agent to exit 3.
	b := attachSession(t, sock, true)
	b.waitOutput("ready")
	b.send("ping\r")
	b.waitOutput("got:ping")
	if code, _ := DecodeExit(b.waitFrame(FrameExit).Payload); code != 3 {
		t.Fatalf("exit %d, want 3", code)
	}
	// Linger, then clean up the socket and exit.
	waitFor(t, 10*time.Second, "holder exit", func() bool {
		_, err := os.Stat(sock)
		return os.IsNotExist(err) && syscall.Kill(info.HolderPID, 0) != nil
	})
	logb, err := os.ReadFile(HolderLogPath(sid))
	if err != nil || !bytes.Contains(logb, []byte("child exited")) {
		t.Fatalf("holder log: %v\n%s", err, logb)
	}
}

func TestBinaryHolderReportsToDaemon(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	fd := startFakeDaemon(t)
	sid := runDetached(t, bin, "--task", "t_test", "--", "/bin/sh", "-c", fakeAgent)
	info, err := Probe(context.Background(), paths.HolderSocket(sid))
	if err != nil {
		t.Fatal(err)
	}
	s := attachSession(t, paths.HolderSocket(sid), true)
	s.waitOutput("ready")
	s.send("x\r")
	s.waitFrame(FrameExit)
	waitFor(t, 5*time.Second, "exit report", func() bool {
		rs := fd.requests()
		return len(rs) > 0 && rs[len(rs)-1].Path == "/v1/sessions/"+sid+"/exit"
	})
	rs := fd.requests()
	var reg api.WrappedSessionBody
	if rs[0].Path != "/v1/sessions/wrapped" || json.Unmarshal(rs[0].Body, &reg) != nil {
		t.Fatalf("first request %s", rs[0].Path)
	}
	if reg.SessionID != sid || reg.HolderPID != info.HolderPID || reg.PID != info.PID || reg.TaskID != "t_test" {
		t.Fatalf("registration %+v vs probe %+v", reg, info)
	}
	var osc, exit int
	for _, r := range rs {
		switch r.Path {
		case "/v1/sessions/" + sid + "/osc":
			osc++
		case "/v1/sessions/" + sid + "/exit":
			if !bytes.Contains(r.Body, []byte(`"exit_code":3`)) {
				t.Fatalf("exit body %s", r.Body)
			}
			exit++
		}
	}
	if osc == 0 || exit != 1 {
		t.Fatalf("osc=%d exit=%d", osc, exit)
	}
}

// ptyProc runs the pilothouse CLI inside a PTY, like a user's terminal.
type ptyProc struct {
	t    *testing.T
	cmd  *exec.Cmd
	ptmx *os.File
	mu   sync.Mutex
	out  bytes.Buffer
	done chan struct{}
	err  error
}

func startInPTY(t *testing.T, bin string, args ...string) *ptyProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	p := &ptyProc{t: t, cmd: cmd, ptmx: ptmx, done: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			answerQueries(ptmx, buf[:n])
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
		ptmx.Close()
		<-readDone
	})
	return p
}

// answerQueries replies to terminal queries the way a real terminal
// does. Libraries that probe the terminal at startup (termenv's
// background-colour query) otherwise wait for seconds.
func answerQueries(w *os.File, b []byte) {
	if bytes.Contains(b, []byte("\x1b]11;?")) {
		_, _ = w.Write([]byte("\x1b]11;rgb:0000/0000/0000\x1b\\"))
	}
	if bytes.Contains(b, []byte("\x1b[6n")) {
		_, _ = w.Write([]byte("\x1b[1;1R"))
	}
}

func (p *ptyProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *ptyProc) waitOutput(s string) {
	p.t.Helper()
	waitFor(p.t, 10*time.Second, "terminal output "+strings.TrimSpace(s), func() bool { return strings.Contains(p.output(), s) })
}

func (p *ptyProc) write(s string) {
	p.t.Helper()
	if _, err := p.ptmx.Write([]byte(s)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *ptyProc) exitCode() int {
	p.t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.t.Fatalf("process did not exit; output so far:\n%q", p.output())
	}
	var ee *exec.ExitError
	if errors.As(p.err, &ee) {
		return ee.ExitCode()
	}
	if p.err != nil {
		p.t.Fatal(p.err)
	}
	return 0
}

// `pilothouse run` in a terminal: output, input and the agent's exit code.
func TestBinaryRunInTerminal(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	p := startInPTY(t, bin, "run", "--", "/bin/sh", "-c", fakeAgent)
	p.waitOutput("ready")
	p.write("ping\r")
	p.waitOutput("got:ping")
	if code := p.exitCode(); code != 3 {
		t.Fatalf("pilothouse run exited %d, want the agent's 3; output %q", code, p.output())
	}

	// An agent that is done before the terminal attaches: its output is
	// replayed and its exit code still propagates.
	q := startInPTY(t, bin, "run", "--", "/bin/sh", "-c", "echo quick; exit 4")
	q.waitOutput("quick")
	if code := q.exitCode(); code != 4 {
		t.Fatalf("pilothouse run exited %d, want 4; output %q", code, q.output())
	}
}

// `pilothouse attach`: detach key, literal ctrl+\, reattach by prefix.
func TestBinaryAttachDetachKey(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	// The agent reads its terminal raw, so a forwarded ctrl+\ arrives as
	// a byte (0x1c) instead of SIGQUIT, and prints it in hex.
	script := `stty raw -echo; echo up; ` +
		`b=$(dd bs=1 count=1 2>/dev/null | od -An -tx1 | tr -d ' \n'); printf 'byte:%s\r\n' "$b"; ` +
		`stty sane; read l; echo "line:$l"; exit 0`
	sid := runDetached(t, bin, "-n", "keys", "--", "/bin/sh", "-c", script)

	// Detach with a single ctrl+\, attaching by an id prefix without "s_".
	a := startInPTY(t, bin, "attach", "--replay", sid[2:7])
	a.waitOutput("up")
	a.write("\x1c")
	a.waitOutput("detached from keys (" + sid + ") — reattach: pilothouse attach " + sid)
	if code := a.exitCode(); code != 0 {
		t.Fatalf("attach exited %d after detach", code)
	}
	if _, err := Probe(context.Background(), paths.HolderSocket(sid)); err != nil {
		t.Fatalf("holder gone after detach: %v", err)
	}

	// Reattach; a double ctrl+\ reaches the agent as one 0x1c.
	// Replayed output is only printed once the terminal is in raw mode;
	// before that the tty would turn ctrl+\ into SIGQUIT.
	b := startInPTY(t, bin, "attach", "--replay", sid)
	b.waitOutput("up")
	b.write("\x1c\x1c")
	b.waitOutput("byte:1c")
	b.write("hello\r")
	b.waitOutput("line:hello")
	if code := b.exitCode(); code != 0 {
		t.Fatalf("attach exited %d; output %q", code, b.output())
	}
}

// A second attach takes over from the first, which says why.
func TestBinaryAttachTakeover(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	sid := runDetached(t, bin, "-n", "shared", "--", "/bin/sh", "-c", `echo up; read l; exit 5`)
	a := startInPTY(t, bin, "attach", "--replay", sid)
	a.waitOutput("up")
	b := startInPTY(t, bin, "attach", "--replay", sid)
	a.waitOutput("attached from another terminal")
	if code := a.exitCode(); code != 0 {
		t.Fatalf("displaced attach exited %d", code)
	}
	b.waitOutput("up")
	b.write("bye\r")
	if code := b.exitCode(); code != 5 {
		t.Fatalf("attach exited %d, want 5", code)
	}
}

func TestBinaryAttachErrors(t *testing.T) {
	testEnv(t)
	bin := pilothouseBin(t)
	out, err := exec.Command(bin, "attach", "s_nothere").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no running session matches") {
		t.Fatalf("attach to nothing: %v %s", err, out)
	}
	out, err = exec.Command(bin, "run", "-d", "--", "definitely-not-a-command-xyz").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "command not found") {
		t.Fatalf("run missing command: %v %s", err, out)
	}
}

package tui

// A real-PTY smoke test: run the inbox against a fake daemon on a Unix
// socket inside a pseudo-terminal, feed the raw output through a small
// VT emulator and capture the screen as text. It is skipped unless
// PILOTHOUSE_SCREEN=1 because its purpose is a human look at the result:
//
//	PILOTHOUSE_SCREEN=1 PILOTHOUSE_SCREEN_OUT=/tmp/screens go test -run TestScreen -v ./internal/tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
)

// vt is a minimal terminal emulator: enough of ECMA-48 for Bubble Tea's
// renderer (cursor movement, erase, alt screen) to rebuild the screen.
type vt struct {
	mu   sync.Mutex
	w, h int
	cell [][]string
	x, y int
	buf  []byte
	sgr  int // colour/style sequences seen, to confirm styling reached the PTY
}

func newVT(w, h int) *vt {
	v := &vt{w: w, h: h}
	v.clear()
	return v
}

func (v *vt) clear() {
	v.cell = make([][]string, v.h)
	for i := range v.cell {
		v.cell[i] = blankRow(v.w)
	}
}

func blankRow(w int) []string {
	r := make([]string, w)
	for i := range r {
		r[i] = " "
	}
	return r
}

func (v *vt) Write(p []byte) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.buf = append(v.buf, p...)
	for len(v.buf) > 0 {
		n := v.step(v.buf)
		if n == 0 {
			break // incomplete sequence: wait for more bytes
		}
		v.buf = v.buf[n:]
	}
	return len(p), nil
}

func (v *vt) lf() {
	v.y++
	if v.y >= v.h {
		copy(v.cell, v.cell[1:])
		v.cell[v.h-1] = blankRow(v.w)
		v.y = v.h - 1
	}
}

// step consumes one token and returns its length (0 = need more input).
func (v *vt) step(b []byte) int {
	switch b[0] {
	case 0x1b:
		if len(b) < 2 {
			return 0
		}
		switch b[1] {
		case '[':
			for i := 2; i < len(b); i++ {
				if b[i] >= 0x40 && b[i] <= 0x7e {
					v.csi(string(b[2:i]), b[i])
					return i + 1
				}
			}
			return 0
		case ']', 'P', '_':
			for i := 2; i < len(b); i++ {
				if b[i] == 0x07 {
					return i + 1
				}
				if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
					return i + 2
				}
			}
			return 0
		case '7', '8', '=', '>', 'M':
			return 2
		default:
			if len(b) < 3 {
				return 0
			}
			return 3
		}
	case '\r':
		v.x = 0
		return 1
	case '\n':
		v.lf()
		return 1
	case '\b':
		v.x = max(0, v.x-1)
		return 1
	case 0x07, 0x0f, 0x0e:
		return 1
	}
	if !utf8.FullRune(b) {
		return 0
	}
	r, n := utf8.DecodeRune(b)
	rw := ansi.StringWidth(string(r))
	if rw == 0 {
		return n
	}
	if v.x >= v.w {
		v.x = 0
		v.lf()
	}
	v.cell[v.y][v.x] = string(r)
	if rw == 2 && v.x+1 < v.w {
		v.cell[v.y][v.x+1] = ""
	}
	v.x += rw
	return n
}

func (v *vt) csi(params string, final byte) {
	if final == 'm' && params != "" && params != "0" {
		v.sgr++
	}
	if strings.HasPrefix(params, "?") {
		if (final == 'h' || final == 'l') && strings.Contains(params, "1049") {
			v.clear()
			v.x, v.y = 0, 0
		}
		return
	}
	var ps []int
	for _, s := range strings.Split(params, ";") {
		n, _ := strconv.Atoi(s)
		ps = append(ps, n)
	}
	arg := func(i, def int) int {
		if i < len(ps) && ps[i] > 0 {
			return ps[i]
		}
		return def
	}
	switch final {
	case 'A':
		v.y = max(0, v.y-arg(0, 1))
	case 'B':
		v.y = min(v.h-1, v.y+arg(0, 1))
	case 'C':
		v.x = min(v.w-1, v.x+arg(0, 1))
	case 'D':
		v.x = max(0, v.x-arg(0, 1))
	case 'G':
		v.x = min(v.w-1, arg(0, 1)-1)
	case 'H', 'f':
		v.y = min(v.h-1, arg(0, 1)-1)
		v.x = min(v.w-1, arg(1, 1)-1)
	case 'K':
		mode := 0
		if len(ps) > 0 {
			mode = ps[0]
		}
		from, to := v.x, v.w
		switch mode {
		case 1:
			from, to = 0, v.x+1
		case 2:
			from, to = 0, v.w
		}
		for i := from; i < min(to, v.w); i++ {
			v.cell[v.y][i] = " "
		}
	case 'J':
		mode := 0
		if len(ps) > 0 {
			mode = ps[0]
		}
		switch mode {
		case 0:
			for i := v.x; i < v.w; i++ {
				v.cell[v.y][i] = " "
			}
			for y := v.y + 1; y < v.h; y++ {
				v.cell[y] = blankRow(v.w)
			}
		case 2, 3:
			v.clear()
		}
	case 'L': // insert lines
		n := arg(0, 1)
		for i := 0; i < n; i++ {
			copy(v.cell[v.y+1:], v.cell[v.y:v.h-1])
			v.cell[v.y] = blankRow(v.w)
		}
	case 'M': // delete lines
		n := arg(0, 1)
		for i := 0; i < n; i++ {
			copy(v.cell[v.y:], v.cell[v.y+1:])
			v.cell[v.h-1] = blankRow(v.w)
		}
	}
}

func (v *vt) Screen() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	lines := make([]string, v.h)
	for i, row := range v.cell {
		lines[i] = strings.TrimRight(strings.Join(row, ""), " ")
	}
	return strings.Join(lines, "\n")
}

// screenDaemon serves the fixture over a real Unix socket.
func screenDaemon(t *testing.T, snap api.Snapshot) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "pilothouse.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(snap)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("POST /v1/requests/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		var b api.DecideBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		_ = json.NewEncoder(w).Encode(api.Request{ID: r.PathValue("id"), Status: api.RequestDecided,
			Decision: &api.Decision{Behavior: b.Behavior, Scope: b.Scope, By: r.Header.Get(client.ClientHeader)}})
	})
	mux.HandleFunc("POST /v1/sessions/{id}/seen", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(func() {
		srv.CloseClientConnections() // end held event streams
		srv.Close()
	})
	return sock
}

// screenRenderer styles for a dark terminal without asking: the emulator
// doesn't answer OSC 11, and Run's up-front query would wait for it.
func screenRenderer(w io.Writer) *lipgloss.Renderer {
	r := lipgloss.NewRenderer(w)
	r.SetHasDarkBackground(true)
	return r
}

func TestScreen(t *testing.T) {
	if os.Getenv("PILOTHOUSE_SCREEN") == "" {
		t.Skip("set PILOTHOUSE_SCREEN=1 to capture real-PTY screens")
	}
	t.Setenv("PILOTHOUSE_USER_HOME", "/home/dev")
	outDir := os.Getenv("PILOTHOUSE_SCREEN_OUT")
	for _, sz := range []struct{ w, h int }{{120, 35}, {70, 30}} {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			sock := screenDaemon(t, fixtureSnapshot())
			ptmx, tty, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer ptmx.Close()
			if err := pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(sz.w), Rows: uint16(sz.h)}); err != nil {
				t.Fatal(err)
			}
			term := newVT(sz.w, sz.h)
			go func() { _, _ = io.Copy(term, ptmx) }()

			start := time.Now()
			m := New(Options{
				Backend:  client.NewSocket(sock).WithClientName("tui"),
				Exe:      "/bin/false",
				Now:      func() time.Time { return t0.Add(time.Since(start)) },
				Renderer: screenRenderer(tty),
			})
			p := tea.NewProgram(m, tea.WithInput(tty), tea.WithOutput(tty), tea.WithAltScreen())
			done := make(chan error, 1)
			go func() { _, err := p.Run(); done <- err }()

			shot := func(name string) {
				time.Sleep(700 * time.Millisecond)
				s := term.Screen()
				term.mu.Lock()
				sgr := term.sgr
				term.mu.Unlock()
				t.Logf("%s %dx%d (%d style sequences so far):\n%s", name, sz.w, sz.h, sgr, s)
				if outDir != "" {
					_ = os.MkdirAll(outDir, 0o755)
					_ = os.WriteFile(filepath.Join(outDir, fmt.Sprintf("%s_%dx%d.txt", name, sz.w, sz.h)), []byte(s+"\n"), 0o644)
				}
			}
			key := func(s string) {
				_, _ = ptmx.Write([]byte(s))
				// A lone ESC followed quickly by a key reads as alt+key.
				time.Sleep(150 * time.Millisecond)
			}

			shot("01-inbox")
			key("jj")
			shot("02-question")
			key("k")
			key("\r")
			shot("03-expanded")
			key("g")
			key("n")
			key("use a PR")
			shot("04-deny")
			key("\x1b")
			key("y")
			shot("05-allowed")
			key("?")
			shot("06-help")
			key("\x1b")
			key("q")
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("inbox did not quit")
			}
			tty.Close()
		})
	}
}

// TestBackgroundQueryPTY checks the inbox's one terminal query against a
// terminal that answers (light background) and one that never does.
func TestBackgroundQueryPTY(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	for _, answer := range []bool{true, false} {
		ptmx, tty, err := pty.Open()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			buf := make([]byte, 64)
			n, _ := ptmx.Read(buf)
			if answer && strings.Contains(string(buf[:n]), "\x1b]11;?") {
				_, _ = ptmx.Write([]byte("\x1b]11;rgb:ffff/ffff/ffff\x1b\\\x1b[?62;22c"))
			}
			_, _ = io.Copy(io.Discard, ptmx)
		}()
		start := time.Now()
		dark := darkBackground(tty, tty, queryTimeout)
		took := time.Since(start)
		tty.Close()
		ptmx.Close()
		if answer && (dark || took > 200*time.Millisecond) {
			t.Errorf("answering terminal: dark=%v took %v", dark, took)
		}
		if !answer && (!dark || took > queryTimeout+200*time.Millisecond) {
			t.Errorf("silent terminal: dark=%v took %v", dark, took)
		}
	}
}

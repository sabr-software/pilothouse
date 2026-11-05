package notify

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A click runs tabCmd + openCmd in /bin/sh. These run that text in a real
// shell, with stand-ins for osascript and open that record their arguments.
func TestClickTriesTheTabFirst(t *testing.T) {
	const origin, local = "https://mac.example.ts.net", "http://127.0.0.1:7517"
	hostile := origin + "/s/x';touch pwned;'$(touch pwned)"
	run := func(t *testing.T, osascript string, wait int) (tab, opened []string) {
		t.Helper()
		dir := t.TempDir()
		fake := func(name, then string) string {
			p := filepath.Join(dir, name)
			body := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >" + p + ".args\n" + then + "\n"
			if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			return p
		}
		cmd := tabCmd(hostile, Opener{Bundle: "com.apple.Safari", Origins: []string{origin, local}}) + openCmd("-b", "com.apple.Safari", hostile)
		cmd = strings.ReplaceAll(cmd, "/usr/bin/osascript", fake("osascript", osascript))
		cmd = strings.ReplaceAll(cmd, "/usr/bin/open", fake("open", "exit 0"))
		cmd = strings.ReplaceAll(cmd, "sleep "+strconv.Itoa(tabWait), "sleep "+strconv.Itoa(wait))
		sh := exec.Command("/bin/sh", "-c", cmd)
		sh.Dir = dir
		if out, err := sh.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
			t.Fatal("the link ran as shell")
		}
		read := func(name string) []string {
			b, err := os.ReadFile(filepath.Join(dir, name+".args"))
			if err != nil {
				return nil
			}
			return strings.Split(strings.TrimSpace(string(b)), "\n")
		}
		return read("osascript"), read("open")
	}

	t.Run("a tab shows the page", func(t *testing.T) {
		tab, opened := run(t, "exit 0", tabWait)
		if opened != nil {
			t.Fatalf("opened a new tab too: %q", opened)
		}
		got := tab[len(tab)-3:]
		if got[0] != strings.TrimPrefix(hostile, origin) || got[1] != origin || got[2] != local {
			t.Fatalf("osascript argv ends %q", got)
		}
	})
	t.Run("no tab, or permission refused: a new tab", func(t *testing.T) {
		if _, opened := run(t, "exit 1", tabWait); strings.Join(opened, " ") != "-b com.apple.Safari "+hostile {
			t.Fatalf("open = %q", opened)
		}
	})
	t.Run("an unanswered prompt: a new tab after the wait", func(t *testing.T) {
		start := time.Now()
		if _, opened := run(t, "exec sleep 20", 1); strings.Join(opened, " ") != "-b com.apple.Safari "+hostile {
			t.Fatalf("open = %q", opened)
		}
		if took := time.Since(start); took > 15*time.Second {
			t.Fatalf("took %v", took)
		}
	})
}

// Only the browsers AppleScript can reach try a tab, by bundle id or by the
// name in notify.desktop_browser.
func TestTabCmdBrowsers(t *testing.T) {
	origins := []string{"https://mac.example.ts.net"}
	link := origins[0] + "/s/s_1"
	for _, c := range []struct {
		o    Opener
		want string
	}{
		{Opener{Bundle: "com.apple.Safari", Origins: origins}, "set current tab of w to t"},
		{Opener{App: "Safari", Origins: origins}, "set current tab of w to t"},
		{Opener{Bundle: "com.google.Chrome", Origins: origins}, chromiumPick},
		{Opener{App: "Microsoft Edge", Origins: origins}, chromiumPick},
		{Opener{Bundle: "org.mozilla.firefox", Origins: origins}, ""},
		{Opener{App: "Some Browser", Origins: origins}, ""},
		{Opener{Bundle: "com.apple.Safari"}, ""}, // no web app address
	} {
		got := tabCmd(link, c.o)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%+v: %q", c.o, got)
		}
	}
	if cmd := tabCmd(link, Opener{Bundle: "com.google.Chrome", Origins: origins}); !strings.Contains(cmd, `application id "com.google.Chrome"`) || strings.Contains(cmd, "%s") {
		t.Fatalf("blanks: %s", cmd)
	}
	args := TerminalNotifierArgs("t", "b", "g", link, Opener{Bundle: "com.apple.Safari", Origins: origins})
	if len(args) != 8 || args[6] != "-execute" || !strings.Contains(args[7], "osascript") || !strings.Contains(args[7], "/usr/bin/open -b") {
		t.Fatalf("args = %q", args)
	}
}

// The AppleScript compiles against Safari's own dictionary.
func TestTabProgramCompiles(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS")
	}
	var src strings.Builder
	for _, l := range tabProgram {
		switch {
		case strings.HasPrefix(l, "tell "):
			l = strings.ReplaceAll(l, "%s", "com.apple.Safari")
		case l == "%s":
			l = tabBrowsers[0].pick
		}
		src.WriteString(l + "\n")
	}
	out := filepath.Join(t.TempDir(), "tab.scpt")
	c := exec.Command("/usr/bin/osacompile", "-o", out)
	c.Stdin = strings.NewReader(src.String())
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
}

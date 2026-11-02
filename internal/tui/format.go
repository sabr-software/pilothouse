package tui

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/paths"
)

// Age formats how long something has been waiting. Seconds are shown for
// the first ten minutes because that is when a waiting agent matters most.
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 10*time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ShortPath replaces the user's home directory with ~.
func ShortPath(p string) string {
	home := paths.UserHome()
	if home != "" && home != "/" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}

// oneLine collapses whitespace so multi-line commands fit in a row.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.TrimSpace(s)
}

// fit truncates or right-pads s (which may contain ANSI) to exactly w cells.
func fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	if pad := w - lipgloss.Width(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// trunc shortens plain text to w cells.
func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// wrap breaks plain text into lines of at most w cells, hard-wrapping
// long words (commands often have no spaces to break at).
func wrap(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		out = append(out, strings.Split(ansi.Wrap(para, w, " /-"), "\n")...)
	}
	return out
}

// requestWhat is the exact thing a request wants to do: the shell
// command, else the file(s), else the adapter's summary.
func requestWhat(r api.Request) string {
	switch {
	case r.Command != "":
		return "$ " + oneLine(r.Command)
	case len(r.Paths) > 0:
		s := r.Tool + " " + ShortPath(r.Paths[0])
		if len(r.Paths) > 1 {
			s += fmt.Sprintf(" +%d", len(r.Paths)-1)
		}
		return s
	case r.Summary != "":
		return oneLine(r.Summary)
	}
	return r.Tool
}

func agentName(a api.AgentKind) string {
	if a == "" {
		return "agent"
	}
	return string(a)
}

// where is "<title> · <branch>" for a session.
func where(s *api.Session, cwd string) string {
	title := ""
	branch := ""
	if s != nil {
		title, branch = s.Title, s.Branch
		if cwd == "" {
			cwd = s.Cwd
		}
	}
	if title == "" && cwd != "" {
		title = filepath.Base(cwd)
	}
	if title == "" {
		title = "?"
	}
	if branch != "" {
		return title + " · " + branch
	}
	return title
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// whereFit is where() for a column of w cells: when "title · branch"
// doesn't fit, the branch goes first so the repo stays readable.
func whereFit(s *api.Session, cwd string, w int) string {
	full := where(s, cwd)
	if lipgloss.Width(full) <= w || s == nil || s.Branch == "" {
		return full
	}
	c := *s
	c.Branch = ""
	return where(&c, cwd)
}

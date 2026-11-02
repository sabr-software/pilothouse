package hookkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabr-software/pilothouse/internal/adapters"
)

// Plan is a computed edit of one JSON hooks file.
type Plan struct {
	File   File
	Result Result
}

// PlanJSON reads path and computes the edit that makes its hooks match
// specs (nil specs: remove ours). It never writes.
func PlanJSON(path string, specs []Spec, owned Owned) (Plan, error) {
	f, err := ReadFile(path)
	if err != nil {
		return Plan{}, err
	}
	res, err := Apply(f.Data, specs, owned)
	if err != nil {
		return Plan{File: f}, fmt.Errorf("%s: %w; refusing to modify it (fix or move the file, then retry)", path, err)
	}
	return Plan{File: f, Result: res}, nil
}

// Change describes the plan; Commit fills in the backup path.
func (p Plan) Change(summary string) adapters.Change {
	return adapters.Change{
		Path:    p.File.Path,
		Changed: p.Result.Changed,
		Before:  string(p.File.Data),
		After:   string(p.Result.Doc),
		Summary: summary,
	}
}

// Commit writes the plan if it changes anything. Uninstalling from a file
// that does not exist is a no-op, never a create.
func (p Plan) Commit(ch *adapters.Change, now time.Time) error {
	if !p.Result.Changed {
		return nil
	}
	backup, err := Commit(p.File, p.Result.Doc, now)
	ch.Backup = backup
	return err
}

// Problems lists reasons hooks found in a file would not work: commands
// whose program is missing or not executable (pilothouse moved or deleted).
func Problems(found []Found) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range found {
		prog := commandProgram(f.Command)
		if prog == "" || seen[prog] {
			continue
		}
		seen[prog] = true
		if !filepath.IsAbs(prog) {
			continue // resolved through PATH at run time
		}
		st, err := os.Stat(prog)
		switch {
		case errors.Is(err, os.ErrNotExist):
			out = append(out, fmt.Sprintf("hook command points to %s, which does not exist; run `pilothouse hooks install` again", prog))
		case err == nil && st.Mode()&0o111 == 0:
			out = append(out, fmt.Sprintf("hook command %s is not executable", prog))
		}
	}
	return out
}

// commandProgram extracts the program from a command ShimCommand built.
func commandProgram(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, "'") {
		var b strings.Builder
		for i := 1; i < len(cmd); i++ {
			switch {
			case strings.HasPrefix(cmd[i:], `'\''`):
				b.WriteByte('\'')
				i += 3
			case cmd[i] == '\'':
				return b.String()
			default:
				b.WriteByte(cmd[i])
			}
		}
		return ""
	}
	if f := strings.Fields(cmd); len(f) > 0 {
		return f[0]
	}
	return ""
}

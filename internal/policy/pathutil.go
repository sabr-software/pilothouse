package policy

import (
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// expandHome replaces a leading "~", "$HOME" or "${HOME}" with home.
// "~user" forms are left alone: resolving other users' homes from a
// policy file would be surprising.
func expandHome(p, home string) string {
	if home == "" {
		return p
	}
	for _, pre := range []string{"~", "${HOME}", "$HOME"} {
		if p == pre {
			return home
		}
		if strings.HasPrefix(p, pre+"/") {
			return home + p[len(pre):]
		}
	}
	return p
}

// absPath makes a tool path absolute the way the agent would resolve it:
// "~" expanded, relative paths joined to cwd, then cleaned.
func absPath(p, cwd, home string) string {
	if p == "" {
		return ""
	}
	p = expandHome(p, home)
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(expandHome(cwd, home), p)
	}
	return filepath.Clean(p)
}

// pathPattern turns a user-written path glob into one matched against
// absolute paths: "~" is expanded, and a relative pattern matches at any
// depth ("*.pem" → "**/*.pem", ".env*" → "**/.env*").
func pathPattern(p, home string) string {
	p = expandHome(p, home)
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "**") {
		return p
	}
	return "**/" + strings.TrimPrefix(p, "./")
}

// pathPatterns is what a rule's path, cwd or repo pattern compiles to. The
// pattern is case-folded where the file system ignores case (values are
// folded the same way before matching). When the directories it names
// before its first wildcard are reached through a symlink (/tmp on macOS),
// the pattern is repeated under the real directory, so a rule written with
// either spelling holds for the file.
func pathPatterns(p, home string) []string {
	p = pathPattern(p, home)
	out := []string{foldPath(p)}
	if alt := realPattern(p); alt != "" && foldPath(alt) != out[0] {
		out = append(out, foldPath(alt))
	}
	return out
}

func realPattern(p string) string {
	if !strings.HasPrefix(p, "/") {
		return ""
	}
	i := strings.IndexAny(p, `*?{\`)
	if i < 0 {
		if r, changed := resolveSymlinks(filepath.Clean(p)); changed {
			return EscapeGlob(r)
		}
		return ""
	}
	j := strings.LastIndexByte(p[:i], '/')
	if j <= 0 {
		return ""
	}
	r, changed := resolveSymlinks(p[:j])
	if !changed {
		return ""
	}
	return EscapeGlob(r) + p[j:]
}

// caseInsensitiveFS is true where the default filesystem ignores case
// (APFS/HFS+ on macOS), so "~/.PILOTHOUSE" and "~/.pilothouse" are the same
// directory for protected-path checks.
var caseInsensitiveFS = runtime.GOOS == "darwin"

func foldPath(p string) string {
	if caseInsensitiveFS {
		return strings.ToLower(p)
	}
	return p
}

// underAny reports whether p (absolute, cleaned, folded) equals or is
// inside one of roots (same form).
func underAny(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || (strings.HasPrefix(p, r) && (strings.HasSuffix(r, "/") || p[len(r)] == '/')) {
			return true
		}
	}
	return false
}

// protectedSet is what the self-protection rule guards. Literal entries
// are roots: the path itself and everything under it. Entries with glob
// syntax (adapters use "**/.claude/settings.json" for project settings in
// any repo) are path-style globs; they too cover the matched path and
// everything under it. Immutable once built, so it is shared freely.
type protectedSet struct {
	roots []string // absolute, cleaned, folded
	globs []*glob  // compiled from folded, absolute-or-"**/" patterns
	// except are carve-outs from broader roots (same form as roots): a
	// path under one is protected only by a root that is itself inside
	// the carve-out (a worktree's own .claude/settings.json) or by a glob.
	except []string
}

// has reports whether p (absolute, cleaned, folded) is protected.
func (s *protectedSet) has(p string) bool {
	if s == nil {
		return false
	}
	if s.root(p) != "" {
		return true
	}
	for _, g := range s.globs {
		if g.match(p) {
			return true
		}
		// "Under a match": try each ancestor. Globs keep their literal
		// suffix, so most of these are rejected by one HasSuffix.
		for i := len(p) - 1; i > 0; i-- {
			if p[i] == '/' && g.match(p[:i]) {
				return true
			}
		}
	}
	return false
}

// root returns the literal root that protects p, the most specific one
// when several do, or "" when none does (globs are not consulted).
func (s *protectedSet) root(p string) string {
	if s == nil {
		return ""
	}
	exempt := underAny(p, s.except)
	best := ""
	for _, r := range s.roots {
		if !underAny(p, []string{r}) {
			continue
		}
		if exempt && !underAny(r, s.except) {
			continue // a broad root (~/.pilothouse) that the carve-out opens
		}
		if len(r) > len(best) {
			best = r
		}
	}
	return best
}

// patternHit reports what a shell pattern (absolute, cleaned, not folded;
// globs and brace lists as the agent wrote them) can expand to that is
// protected: a literal root or something inside one, judged from the
// spelling alone, or a file matching a protected glob that exists now.
// dots: * and ? also match a leading dot. It returns "" for no hit.
// unchecked: the pattern stands for more names than are looked at, so
// nobody can say it is harmless.
func (s *protectedSet) patternHit(pattern string, dots bool) (hit string, unchecked bool) {
	if s == nil {
		return "", false
	}
	alts := braceExpand(pattern, maxBraceAlts+1)
	if len(alts) > maxBraceAlts {
		return "", true
	}
	walk := globWalk{left: maxPatternNames, dots: dots, found: func(p string) bool { return s.has(foldPath(p)) }}
	for _, alt := range alts {
		folded := foldPath(alt)
		segs := strings.Split(strings.Trim(folded, "/"), "/")
		if len(segs) > maxPatternSegs {
			continue
		}
		// A pattern rooted in a carve-out (a task's worktree) is only
		// answerable to the roots inside it.
		exempt := underAny(literalDir(folded), s.except)
		for _, r := range s.roots {
			if exempt && !underAny(r, s.except) {
				continue
			}
			if segsMatch(segs, strings.Split(strings.Trim(r, "/"), "/"), dots) {
				return r, false
			}
		}
		if len(s.globs) == 0 || strings.Contains(alt, "**") || !strings.ContainsAny(alt, "*?[") {
			continue
		}
		// Protected globs ("**/.claude/settings.json") are matched
		// against what the pattern names on disk right now.
		walk.run("", strings.Split(strings.Trim(alt, "/"), "/"))
		if walk.hit != "" || walk.left < 0 {
			return walk.hit, walk.hit == ""
		}
	}
	return "", false
}

// heldBy returns a literal root that exists inside the directory path
// names (absolute, cleaned, not folded; globs and brace lists allowed), or
// "". Protected globs are not consulted: a repo's own settings files are
// literal roots already.
func (s *protectedSet) heldBy(path string, dots bool) string {
	if s == nil {
		return ""
	}
	for _, alt := range braceExpand(path, maxBraceAlts) {
		folded := foldPath(alt)
		segs := pathSegs(folded)
		exempt := underAny(literalDir(folded), s.except)
	roots:
		for _, r := range s.roots {
			rs := pathSegs(r)
			if len(segs) >= len(rs) || (exempt && !underAny(r, s.except)) {
				continue
			}
			for i, seg := range segs {
				if !segMatch(seg, rs[i], dots) {
					continue roots
				}
			}
			if _, err := os.Lstat(r); err == nil {
				return r
			}
		}
	}
	return ""
}

// pathSegs splits an absolute path into its names; "/" has none.
func pathSegs(p string) []string {
	if p = strings.Trim(p, "/"); p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

const (
	maxBraceAlts   = 256
	maxPatternSegs = 64
)

// maxPatternNames is how many directory entries are read for one pattern.
var maxPatternNames = 50000

// globWalk expands a shell pattern against the disk, one path segment at a
// time, and stops at the first name found() accepts or when it has read
// more directory entries than it may.
type globWalk struct {
	left  int // entries it may still read; negative: gave up
	dots  bool
	found func(path string) bool
	hit   string
}

// run reports whether the walk is over (a hit, or out of budget).
func (w *globWalk) run(dir string, segs []string) bool {
	for len(segs) > 0 && !strings.ContainsAny(segs[0], "*?[") {
		dir, segs = dir+"/"+segs[0], segs[1:]
	}
	if len(segs) == 0 {
		if _, err := os.Lstat(dir); err == nil && w.found(dir) {
			w.hit = dir
		}
		return w.hit != ""
	}
	if dir == "" {
		dir = "/"
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	if w.left -= len(ents); w.left < 0 {
		return true
	}
	for _, e := range ents {
		if !segMatch(segs[0], e.Name(), w.dots) {
			continue
		}
		if len(segs) > 1 && !e.IsDir() && e.Type()&os.ModeSymlink == 0 {
			continue
		}
		if w.run(strings.TrimSuffix(dir, "/")+"/"+e.Name(), segs[1:]) {
			return true
		}
	}
	return false
}

// literalDir is the directory part of pattern before its first glob.
func literalDir(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 {
		return pattern
	}
	return strings.TrimSuffix(pattern[:strings.LastIndexByte(pattern[:i], '/')+1], "/")
}

// segsMatch reports whether pattern segments p can name the path r or
// something inside it. "**" stands for any number of directories.
func segsMatch(p, r []string, dots bool) bool {
	if len(r) == 0 {
		return true
	}
	if len(p) == 0 {
		return false
	}
	if p[0] == "**" {
		if segsMatch(p[1:], r, dots) {
			return true
		}
		if !dots && strings.HasPrefix(r[0], ".") {
			return false
		}
		return segsMatch(p, r[1:], dots)
	}
	return segMatch(p[0], r[0], dots) && segsMatch(p[1:], r[1:], dots)
}

// segMatch matches one path segment the way a shell does: a leading *, ?
// or [ doesn't match a leading dot unless dots is set.
func segMatch(pat, name string, dots bool) bool {
	if !strings.ContainsAny(pat, "*?[") {
		return pat == name
	}
	if !dots && strings.HasPrefix(name, ".") && strings.ContainsRune("*?[", rune(pat[0])) {
		return false
	}
	ok, err := path.Match(pat, name)
	if err != nil {
		return pat == name // an unclosed [ is an ordinary character
	}
	return ok
}

// braceExpand expands shell brace lists ("a{b,c}d" → "abd", "acd"),
// nested ones too, into at most limit words. A group without a top-level
// comma is literal, as in bash.
func braceExpand(s string, limit int) []string {
	out := []string{}
	var walk func(s string)
	walk = func(s string) {
		if len(out) >= limit {
			return
		}
		for open := 0; open < len(s); open++ {
			if s[open] != '{' {
				continue
			}
			depth, commas := 0, []int{}
			for i := open; i < len(s); i++ {
				switch s[i] {
				case '{':
					depth++
				case ',':
					if depth == 1 {
						commas = append(commas, i)
					}
				case '}':
					depth--
				}
				if depth > 0 {
					continue
				}
				if len(commas) == 0 {
					break // literal braces: look for a later group
				}
				from := open + 1
				for _, c := range append(commas, i) {
					walk(s[:open] + s[from:c] + s[i+1:])
					from = c + 1
				}
				return
			}
		}
		out = append(out, s)
	}
	walk(s)
	return out
}

// withRoots returns a copy of s with extra literal roots (same form).
func (s *protectedSet) withRoots(extra ...string) *protectedSet {
	return &protectedSet{roots: append(slices.Clip(s.roots), extra...), globs: s.globs, except: s.except}
}

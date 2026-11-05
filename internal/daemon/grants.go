package daemon

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/policy"
	"github.com/sabr-software/pilothouse/internal/risk"
)

// Session grants ("allow for this session", BUILD-SPEC §5).
//
// A shell grant covers a command *key*: the program plus its subcommand
// ("git push"), found after skipping the program's global flags and their
// values, so `git -c user.name=x commit` grants "git commit" — never all of
// git. When the subcommand can't be determined safely (an unknown flag that
// might take a value, a path or quoted word where the subcommand should
// be), the grant covers only that exact command. A bare program name is
// granted only for a command that was just the program name.

// globalFlags describes a program's options that may come before its
// subcommand: flags that take a separate value, and flags that don't.
// A flag in neither set makes the subcommand undeterminable.
type globalFlags struct {
	value   map[string]bool
	boolean map[string]bool
}

func flagSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// wholeProgramTools create directories or look at metadata, never file
// contents, so "allow for this session" on one call can cover every call of
// the program (`mkdir -p a`, then `mkdir -p b`). Content readers such as cat
// stay exact: allowing `cat README.md` must not allow `cat ~/.ssh/id_ed25519`.
// Deny and ask rules still run first.
var wholeProgramTools = map[string]bool{
	"mkdir": true, "touch": true, "ls": true, "pwd": true, "which": true,
	"tree": true, "stat": true, "du": true, "df": true,
}

// subcommandTools are programs whose second word selects what they do.
var subcommandTools = map[string]globalFlags{
	"git": {
		value: flagSet("-c", "-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--list-cmds"),
		boolean: flagSet("-p", "-P", "--paginate", "--no-pager", "--bare", "--no-replace-objects", "--literal-pathspecs",
			"--glob-pathspecs", "--noglob-pathspecs", "--icase-pathspecs", "--no-optional-locks", "--no-advice"),
	},
	"npm": {
		value:   flagSet("--prefix", "-C", "--userconfig", "--registry", "--cache", "--loglevel", "-w", "--workspace"),
		boolean: flagSet("-g", "--global", "-s", "--silent", "-q", "--quiet", "-d", "--verbose", "-y", "--yes", "--workspaces", "-ws"),
	},
	"pnpm": {
		value:   flagSet("-C", "--dir", "--filter", "-F", "--loglevel"),
		boolean: flagSet("-r", "--recursive", "-w", "--workspace-root", "-s", "--silent", "-g", "--global"),
	},
	"yarn": {
		value:   flagSet("--cwd"),
		boolean: flagSet("-s", "--silent", "--verbose"),
	},
	"bun":   {boolean: flagSet("--silent")},
	"cargo": {value: flagSet("--color", "--config", "-Z", "-C"), boolean: flagSet("-v", "-vv", "-q", "--quiet", "--verbose", "--offline", "--frozen", "--locked")},
	"go":    {value: flagSet("-C")},
	"docker": {
		value:   flagSet("-c", "--context", "-H", "--host", "--config", "-l", "--log-level", "--tlscacert", "--tlscert", "--tlskey"),
		boolean: flagSet("-D", "--debug", "--tls", "--tlsverify"),
	},
	"kubectl": {
		value:   flagSet("-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server", "--as", "-v"),
		boolean: flagSet("--insecure-skip-tls-verify"),
	},
	"helm":      {value: flagSet("-n", "--namespace", "--kube-context", "--kubeconfig"), boolean: flagSet("--debug")},
	"gh":        {value: flagSet("-R", "--repo")},
	"terraform": {value: flagSet("-chdir")},
	"aws":       {value: flagSet("--profile", "--region", "--output", "--endpoint-url"), boolean: flagSet("--debug", "--no-cli-pager", "--no-paginate")},
	"gcloud":    {value: flagSet("--project", "--account", "--configuration"), boolean: flagSet("-q", "--quiet")},
	"brew":      {boolean: flagSet("-v", "--verbose", "-d", "--debug")},
	"pip":       {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"pip3":      {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"uv":        {value: flagSet("--directory", "--project"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "--offline")},
	"poetry":    {value: flagSet("-C", "--directory"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "-n", "--no-interaction")},
	"systemctl": {boolean: flagSet("--user", "--system", "-q", "--quiet", "--no-pager")},
	"make":      {value: flagSet("-C", "--directory", "-f", "--file", "--makefile", "-I", "-o", "-W"), boolean: flagSet("-s", "--silent", "-k", "-B", "-n", "--dry-run")},
	"npx":       {value: flagSet("-p", "--package"), boolean: flagSet("-y", "--yes", "--no", "-q", "--quiet")},
	"deno":      {boolean: flagSet("-q", "--quiet")},
}

// commandKey is the grant key of a shell command: "prog sub" when the
// subcommand can be found, "prog" for a command that is only a program
// name. ok is false when neither can be determined safely.
func commandKey(words []string) (string, bool) {
	if len(words) == 0 {
		return "", false
	}
	prog := words[0]
	if len(words) == 1 {
		return prog, !isFlagOrPath(prog)
	}
	if wholeProgramTools[filepath.Base(prog)] && !isFlagOrPath(prog) {
		return prog, true
	}
	flags, known := subcommandTools[filepath.Base(prog)]
	if !known {
		// Unknown program: a plain second word is its subcommand (or
		// target, as for `make deploy`); anything else is undeterminable.
		if isFlagOrPath(words[1]) {
			return "", false
		}
		return prog + " " + words[1], true
	}
	for i := 1; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--":
			return "", false
		case strings.HasPrefix(w, "+") && filepath.Base(prog) == "cargo":
			continue // toolchain selector: cargo +nightly build
		case strings.HasPrefix(w, "-"):
			name, _, inline := strings.Cut(w, "=")
			switch {
			case flags.value[name] && inline:
			case flags.value[name]:
				i++ // skip the flag's value
			case flags.boolean[name] && !inline:
			case inline && strings.HasPrefix(name, "--"):
				// --unknown=value carries its own value.
			default:
				return "", false
			}
		case isFlagOrPath(w):
			return "", false
		default:
			return prog + " " + w, true
		}
	}
	return "", false // only flags: no subcommand
}

// grantPrefix is what an "allow for this session" covers: for shell tools
// the command key, or the exact command when it has none; other tools: the
// whole tool ("").
func grantPrefix(req api.Request) string {
	if req.ToolKind != api.ToolShell {
		return ""
	}
	words := strings.Fields(req.Command)
	if key, ok := commandKey(words); ok {
		return key
	}
	return strings.Join(words, " ")
}

func isFlagOrPath(w string) bool {
	return strings.HasPrefix(w, "-") || strings.ContainsAny(w, "/.~=$") || strings.HasPrefix(w, "\"") || strings.HasPrefix(w, "'")
}

// shellControl are characters that chain or redirect commands. A grant for
// "git push" must not also allow "git push; rm -rf ~", so commands
// containing any of these never match a grant and go back to a person.
const shellControl = ";&|`$<>()\n\\"

// matchGrant is matchGrantAt with no folder known: file readers stay exact.
func matchGrant(grants []api.Grant, tool string, kind api.ToolKind, command string) (api.Grant, bool) {
	return matchGrantAt(grants, tool, kind, command, "", "")
}

// matchGrantAt returns the first grant covering a tool call: a shell call
// matches when its command key equals the grant, or it is exactly the
// granted command, or (grantSteps) every part of a chained line is covered.
func matchGrantAt(grants []api.Grant, tool string, kind api.ToolKind, command, cwd, root string) (api.Grant, bool) {
	if g, ok := matchWhole(grants, tool, kind, command); ok {
		return g, true
	}
	if kind != api.ToolShell {
		return api.Grant{}, false
	}
	return matchSteps(grants, tool, command, cwd, root)
}

// matchWhole matches a call as one unit: a plain command, never a chain.
func matchWhole(grants []api.Grant, tool string, kind api.ToolKind, command string) (api.Grant, bool) {
	var words []string
	var key string
	var keyed bool
	if kind == api.ToolShell {
		words = strings.Fields(command)
		key, keyed = commandKey(words)
	}
	for _, g := range grants {
		if g.Tool != tool {
			continue
		}
		if g.Prefix == "" {
			if kind != api.ToolShell {
				return g, true
			}
			continue // a shell grant always has a prefix; an empty one is malformed
		}
		if strings.ContainsAny(command, shellControl) {
			continue
		}
		prefix := strings.Join(strings.Fields(g.Prefix), " ")
		if (keyed && key == prefix) || strings.Join(words, " ") == prefix {
			return g, true
		}
	}
	return api.Grant{}, false
}

// Chained commands. Agents rarely run one plain command: `cd web && grep -n
// x src/a.ts | head -20; sed -n 1,80p src/b.ts 2>/dev/null`. Each part of a
// line is judged on its own, so one "allow for this session" keeps working
// when the next line differs only in its files.
//
// A line can be covered only when every part is:
//   - cd and ls inside the session's folder, commands that only print
//     (neutralTools) and readers fed by a pipe need no grant;
//   - file readers (readTools) share one grant, readGrant: allowing one
//     allows reading files inside the session's folder with any of them.
//     Allowing `cat README.md` still doesn't allow `cat ~/.ssh/id_ed25519`;
//   - anything else needs its command key, or that exact part, granted.
//
// These always go back to a person: $(…) and backticks, writes through a
// redirection, sudo and other wrappers that change what runs, NAME=value
// prefixes, a pipe into an interpreter, and any line risk.Classify labels
// in grantBlockers. Deny and ask rules run before all of this.

var neutralTools = map[string]bool{"echo": true, "printf": true, "true": true, "false": true, "pwd": true,
	"sleep": true, ":": true, "test": true, "[": true, "date": true, "tr": true, "basename": true, "dirname": true}

var readTools = map[string]bool{"cat": true, "head": true, "tail": true, "sed": true, "grep": true, "egrep": true,
	"fgrep": true, "rg": true, "wc": true, "sort": true, "uniq": true, "cut": true, "nl": true, "jq": true, "diff": true, "find": true}

// plainWrappers run the command after them unchanged.
var plainWrappers = map[string]bool{"time": true, "nohup": true, "nice": true, "timeout": true, "gtimeout": true,
	"command": true, "builtin": true, "stdbuf": true, "if": true, "then": true, "else": true, "elif": true,
	"do": true, "while": true, "until": true, "!": true}

var interpreters = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true,
	"python": true, "python3": true, "node": true, "ruby": true, "perl": true, "php": true, "lua": true, "deno": true, "bun": true}

// grantBlockers are risk labels a chained line or a file read can't carry
// and still run unseen. Labels on one plain granted command don't apply: the
// person saw that command's label when they granted it.
var grantBlockers = map[string]bool{api.RiskDestructive: true, api.RiskForcePush: true, api.RiskDeploy: true,
	api.RiskSecrets: true, api.RiskNetwork: true, api.RiskSudo: true, api.RiskOutsideRepo: true}

// readGrant is the grant prefix for reading files in the session's folder.
// It can't equal a command: no key or command line starts with #.
const readGrant = "#read"

// sedPrint is the only sed script a grant covers: printing lines.
var sedPrint = regexp.MustCompile(`^[0-9$,;p ]+$`)

// grantStep is one part of a line that needs a grant. reader marks
// readGrant.
type grantStep struct {
	key    string
	reader bool
}

// grantSteps reads a shell line into the grants it needs. ok is false when
// part of it can never be covered; steps still lists the parts that can.
func grantSteps(command, cwd, root string) (steps []grantStep, ok bool) {
	line := policy.ParseShell(command)
	ok = len(line.Subs) == 0 && len(line.Commands) > 0
	for _, c := range line.Commands {
		argv, seen := policy.UnwrapCommand(c.Words)
		if !plainPrefix(c.Words, argv, seen) {
			ok = false
			continue
		}
		if len(argv) == 0 {
			continue
		}
		prog := policy.CommandName(argv[0])
		if strings.ContainsAny(argv[0], "$`") || (c.PipedIn && interpreters[prog]) || !plainRedirects(c.Redirects, cwd, root) {
			ok = false
			continue
		}
		switch {
		case prog == "cd":
			if len(argv) != 2 || c.Vars || !insideRoot(argv[1], cwd, root) {
				ok = false
				continue
			}
			cwd = resolveIn(argv[1], cwd)
		case neutralTools[prog]:
		case readTools[prog] && !c.Vars && readerSafe(prog, argv[1:]) && allInside(argv[1:], cwd, root):
			// At the end of a pipe (`| head -20`) it only shapes output.
			if !c.PipedIn {
				steps = append(steps, grantStep{key: readGrant, reader: true})
			}
		case prog == "ls" && allInside(argv[1:], cwd, root):
		default:
			key, keyed := commandKey(argv)
			if !keyed {
				key = strings.Join(argv, " ")
			}
			steps = append(steps, grantStep{key: key})
		}
	}
	return steps, ok
}

// plainPrefix reports that only plain wrappers come before argv: no
// NAME=value (PATH=… changes what runs), no sudo, xargs or env.
func plainPrefix(words, argv, seen []string) bool {
	for _, w := range seen {
		if !plainWrappers[w] {
			return false
		}
	}
	for _, w := range words[:len(words)-len(argv)] {
		if strings.Contains(w, "=") && !strings.HasPrefix(w, "-") {
			return false
		}
	}
	return true
}

// plainRedirects allows dropping output and merging streams, and reading a
// file inside the session's folder. Everything else writes.
func plainRedirects(rs []policy.ShellRedirect, cwd, root string) bool {
	for _, r := range rs {
		switch r.Op {
		case "<<", "<<-", "<<<":
		case "<":
			if !insideRoot(r.Target, cwd, root) {
				return false
			}
		case ">&", "<&":
			if strings.Trim(r.Target, "0123456789-") != "" {
				return false
			}
		default:
			if r.Target != "/dev/null" {
				return false
			}
		}
	}
	return true
}

// readerSafe refuses the options that make a file reader write or run
// something.
func readerSafe(prog string, args []string) bool {
	var pos []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
		}
	}
	switch prog {
	case "sed":
		return slices.Contains(args, "-n") && len(pos) > 0 && sedPrint.MatchString(pos[0])
	case "find":
		for _, a := range args {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
	case "sort":
		for _, a := range args {
			if strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--compress-program") {
				return false
			}
		}
	case "rg":
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") {
				return false
			}
		}
	case "uniq":
		return len(pos) <= 1 // a second name is its output file
	}
	return true
}

// allInside checks every word that could be a path. A search pattern that
// looks like one outside the folder only costs a question.
func allInside(args []string, cwd, root string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			_, v, has := strings.Cut(a, "=")
			if !has {
				continue
			}
			a = v
		}
		if !insideRoot(a, cwd, root) {
			return false
		}
	}
	return true
}

func resolveIn(p, cwd string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

// insideRoot reports that p, as written, stays inside root. It doesn't touch
// the disk: a symlink inside the folder pointing out of it isn't seen.
func insideRoot(p, cwd, root string) bool {
	if root == "" || cwd == "" || strings.HasPrefix(p, "~") {
		return false
	}
	abs := resolveIn(p, cwd)
	return abs == root || strings.HasPrefix(abs, strings.TrimRight(root, "/")+"/")
}

// matchSteps matches a line part by part. The grant returned is the first
// that covers a part.
func matchSteps(grants []api.Grant, tool, command, cwd, root string) (api.Grant, bool) {
	steps, ok := grantSteps(command, cwd, root)
	if !ok || len(steps) == 0 {
		return api.Grant{}, false
	}
	var first api.Grant
	for _, st := range steps {
		i := slices.IndexFunc(grants, func(g api.Grant) bool { return g.Tool == tool && g.Prefix == st.key })
		if i < 0 {
			return api.Grant{}, false
		}
		if first.ID == "" {
			first = grants[i]
		}
	}
	for _, l := range risk.Classify(risk.Call{Tool: tool, Kind: api.ToolShell, Command: command, Cwd: cwd, Repo: root}) {
		if grantBlockers[l.Code] {
			return api.Grant{}, false
		}
	}
	return first, true
}

// grantKeys are the grants an "allow for this session" on req creates: one
// per part of the line. A line with no part to grant falls back to
// grantPrefix.
func grantKeys(req api.Request, root string) []string {
	if req.ToolKind != api.ToolShell {
		return []string{""}
	}
	steps, _ := grantSteps(req.Command, req.Cwd, root)
	var keys []string
	for _, st := range steps {
		if !slices.Contains(keys, st.key) {
			keys = append(keys, st.key)
		}
	}
	if len(keys) == 0 {
		return []string{grantPrefix(req)}
	}
	return keys
}

// grantCovers says in a few words what an "allow for this session" on req
// would cover, for the button: "git status", "reading files in this folder".
func grantCovers(req api.Request, root string) []string {
	if req.ToolKind != api.ToolShell {
		return []string{"every " + req.Tool + " call"}
	}
	steps, _ := grantSteps(req.Command, req.Cwd, root)
	var out []string
	for _, st := range steps {
		s := st.key
		if st.reader {
			s = "reading files in this folder"
		}
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

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
// git. A bare program name is granted only for a command that was just the
// program name.
//
// The key is a convenience, so whenever it would cover more than the person
// was shown, the grant is the exact command instead (sessionKey, widens):
//   - a program not listed here: its second word may be an argument
//     (`rm build`, `ssh host`), not a subcommand;
//   - a global option that changes what runs or where (`git -c
//     core.pager=…`, `make -f …`, `docker -H …`, `git -C` out of the folder);
//   - a risk label the key alone doesn't carry (`git push --force`);
//   - an argument naming a path outside the session's folder.
//
// The same test runs on the later command, so `git push` keeps covering
// `git push origin main` and never covers `git push --force`.

// globalFlags describes a program's options that may come before its
// subcommand: flags that take a separate value, and flags that don't.
// A flag in neither set makes the subcommand undeterminable.
type globalFlags struct {
	value   map[string]bool
	boolean map[string]bool
	// safe are the value options that can't change what runs or where it
	// runs (a log level, a workspace name). Directory options (dirFlags)
	// are judged by their value; any other value option makes a command
	// exact for session grants.
	safe map[string]bool
	// anywhere: the program also accepts its global options after the
	// subcommand (`npm install --registry …`, `make test -f …`).
	anywhere bool
}

// dirFlags name the directory a program works in. Inside the session's
// folder they don't change what a grant covers.
var dirFlags = flagSet("-C", "--git-dir", "--work-tree", "--prefix", "--cwd", "--dir", "--directory", "-chdir", "--project")

// gitSafeConfig are `git -c` settings that can't run a program or move
// where git reads and writes.
var gitSafeConfig = flagSet("user.name", "user.email", "core.quotepath", "commit.gpgsign", "tag.gpgsign",
	"init.defaultbranch", "core.autocrlf", "core.filemode", "core.ignorecase", "diff.renames", "pull.rebase",
	"pull.ff", "merge.ff", "push.default", "push.autosetupremote", "rebase.autostash", "fetch.prune",
	"log.date", "log.decorate", "status.short", "status.branch")

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
		value:    flagSet("--prefix", "-C", "--userconfig", "--registry", "--cache", "--loglevel", "-w", "--workspace"),
		boolean:  flagSet("-g", "--global", "-s", "--silent", "-q", "--quiet", "-d", "--verbose", "-y", "--yes", "--workspaces", "-ws"),
		safe:     flagSet("--loglevel", "-w", "--workspace"),
		anywhere: true,
	},
	"pnpm": {
		value:    flagSet("-C", "--dir", "--filter", "-F", "--loglevel"),
		boolean:  flagSet("-r", "--recursive", "-w", "--workspace-root", "-s", "--silent", "-g", "--global"),
		safe:     flagSet("--filter", "-F", "--loglevel"),
		anywhere: true,
	},
	"yarn": {
		value:    flagSet("--cwd"),
		boolean:  flagSet("-s", "--silent", "--verbose"),
		anywhere: true,
	},
	"bun": {boolean: flagSet("--silent")},
	"cargo": {
		value:    flagSet("--color", "--config", "-Z", "-C"),
		boolean:  flagSet("-v", "-vv", "-q", "--quiet", "--verbose", "--offline", "--frozen", "--locked"),
		safe:     flagSet("--color"),
		anywhere: true,
	},
	"go": {value: flagSet("-C")},
	"docker": {
		value:   flagSet("-c", "--context", "-H", "--host", "--config", "-l", "--log-level", "--tlscacert", "--tlscert", "--tlskey"),
		boolean: flagSet("-D", "--debug", "--tls", "--tlsverify"),
		safe:    flagSet("-l", "--log-level"),
	},
	"kubectl": {
		value:    flagSet("-n", "--namespace", "--context", "--kubeconfig", "--cluster", "--user", "-s", "--server", "--as", "-v"),
		boolean:  flagSet("--insecure-skip-tls-verify"),
		safe:     flagSet("-n", "--namespace", "-v"),
		anywhere: true,
	},
	"helm": {
		value: flagSet("-n", "--namespace", "--kube-context", "--kubeconfig"), boolean: flagSet("--debug"),
		safe: flagSet("-n", "--namespace"), anywhere: true,
	},
	"gh":        {value: flagSet("-R", "--repo")},
	"terraform": {value: flagSet("-chdir")},
	"aws": {
		value: flagSet("--profile", "--region", "--output", "--endpoint-url"), boolean: flagSet("--debug", "--no-cli-pager", "--no-paginate"),
		safe: flagSet("--output"), anywhere: true,
	},
	"gcloud": {
		value: flagSet("--project", "--account", "--configuration"), boolean: flagSet("-q", "--quiet"),
		anywhere: true,
	},
	"brew":      {boolean: flagSet("-v", "--verbose", "-d", "--debug")},
	"pip":       {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"pip3":      {boolean: flagSet("-q", "--quiet", "-v", "--verbose")},
	"uv":        {value: flagSet("--directory", "--project"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "--offline")},
	"poetry":    {value: flagSet("-C", "--directory"), boolean: flagSet("-q", "--quiet", "-v", "--verbose", "-n", "--no-interaction")},
	"systemctl": {boolean: flagSet("--user", "--system", "-q", "--quiet", "--no-pager")},
	"make": {
		value:    flagSet("-C", "--directory", "-f", "--file", "--makefile", "-I", "-o", "-W"),
		boolean:  flagSet("-s", "--silent", "-k", "-B", "-n", "--dry-run"),
		anywhere: true,
	},
	"npx":  {value: flagSet("-p", "--package"), boolean: flagSet("-y", "--yes", "--no", "-q", "--quiet")},
	"deno": {boolean: flagSet("-q", "--quiet")},
}

// More programs whose second word is a subcommand, task or target. They
// have no global options listed, so any option before the subcommand makes
// the command exact.
func init() {
	for _, name := range []string{"bundle", "rails", "rake", "mix", "dotnet", "gradle", "gradlew", "mvn", "mvnw",
		"swift", "zig", "composer", "just", "task", "turbo", "nx", "flutter", "dart", "playwright", "vitest",
		"ruff", "hatch", "pdm", "rye", "pipx", "conda", "mamba", "bazel", "buck2", "nix", "podman", "lerna",
		"rush", "vercel", "wrangler", "supabase", "fly", "flyctl", "heroku", "pulumi", "goreleaser", "astro",
		"next", "vite"} {
		if _, ok := subcommandTools[name]; !ok {
			sessionOnlyTools[name] = true
		}
	}
}

// sessionOnlyTools are keyed by subcommand for session grants only; rule
// suggestions (commandKey) keep treating them as unknown programs.
var sessionOnlyTools = map[string]bool{}

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

// sessionKey is the key a session grant may use for argv: "prog sub" for a
// listed program whose options before the subcommand (and after it, where
// the program reads them there) can't change what runs or where; the
// program alone for wholeProgramTools and one-word commands. ok is false
// for everything else, which is granted as the exact command.
func sessionKey(argv []string, cwd, root string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	prog := argv[0]
	base := filepath.Base(prog)
	if len(argv) == 1 {
		return prog, !isFlagOrPath(prog)
	}
	if wholeProgramTools[base] && !isFlagOrPath(prog) {
		if base == "tree" && slices.ContainsFunc(argv[1:], func(a string) bool { return strings.HasPrefix(a, "-o") || strings.HasPrefix(a, "--output") }) {
			return "", false // tree -o writes a file
		}
		return prog, true
	}
	flags, known := subcommandTools[base]
	if !known && !sessionOnlyTools[base] {
		return "", false
	}
	key := ""
	for i := 1; i < len(argv); i++ {
		w := argv[i]
		switch {
		case w == "--":
			if key == "" {
				return "", false
			}
			return key, true // the rest belongs to what the subcommand runs
		case key == "" && strings.HasPrefix(w, "+") && base == "cargo":
			continue // toolchain selector: cargo +nightly build
		case strings.HasPrefix(w, "-"):
			name, val, inline := strings.Cut(w, "=")
			switch {
			case flags.value[name]:
				if !inline {
					if i+1 >= len(argv) {
						return "", false
					}
					i++
					val = argv[i]
				}
				if !safeGlobalValue(base, name, val, flags, cwd, root) {
					return "", false
				}
			case key != "":
				// An option of the subcommand: widens judges it.
			case flags.boolean[name] && !inline:
			default:
				// Unknown before the subcommand: it may take a value, or
				// change what runs (git --exec-path=…).
				return "", false
			}
		case key != "":
		case isFlagOrPath(w):
			return "", false
		default:
			key = prog + " " + w
			if !flags.anywhere {
				return key, true
			}
		}
	}
	return key, key != ""
}

// safeGlobalValue reports that a global option's value keeps a command
// inside what its key describes.
func safeGlobalValue(base, name, val string, flags globalFlags, cwd, root string) bool {
	switch {
	case base == "git" && name == "-c":
		k, v, _ := strings.Cut(val, "=")
		k = strings.ToLower(k)
		if k == "core.pager" {
			return v == "" || v == "cat"
		}
		return gitSafeConfig[k] || strings.HasPrefix(k, "color.") || strings.HasPrefix(k, "advice.")
	case dirFlags[name]:
		return !outsideFolder(val, false, cwd, root)
	}
	return flags.safe[name]
}

// outsideFolder reports that a word, read as a path, may leave the
// session's folder. With no folder known, only relative paths that don't
// climb count as inside.
func outsideFolder(p string, glob bool, cwd, root string) bool {
	if p == "" || p == "/dev/null" {
		return false
	}
	if strings.HasPrefix(p, "~") || (glob && expandsElsewhere(p)) {
		return true
	}
	if root != "" && cwd != "" {
		return !insideRoot(p, cwd, root)
	}
	return filepath.IsAbs(p) || slices.Contains(strings.Split(p, "/"), "..")
}

// expandsElsewhere reports that the shell may expand an unquoted word into
// names that aren't written in it: brace lists (`{/etc/passwd,a}`), and
// globs in a name starting with a dot, which can match `..`.
func expandsElsewhere(p string) bool {
	if strings.Contains(p, "{") {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") && strings.ContainsAny(part, "*?[") {
			return true
		}
	}
	return false
}

// widens reports that argv does more than its key says: it carries a
// blocking risk label the key alone doesn't, or names a path outside the
// session's folder.
func widens(argv []string, globs []bool, key, cwd, root string) bool {
	keyLabels := map[string]bool{}
	for _, l := range risk.Classify(risk.Call{Kind: api.ToolShell, Command: key, Cwd: cwd, Repo: root}) {
		keyLabels[l.Code] = true
	}
	for _, l := range risk.Classify(risk.Call{Kind: api.ToolShell, Command: shellJoin(argv, globs), Cwd: cwd, Repo: root}) {
		if grantBlockers[l.Code] && !keyLabels[l.Code] {
			return true
		}
	}
	if wholeProgramTools[filepath.Base(argv[0])] {
		return false // metadata only; writes outside carry outside_repo above
	}
	for i, a := range argv[1:] {
		glob := i+1 < len(globs) && globs[i+1]
		_, v, has := strings.Cut(a, "=")
		switch {
		case has:
			a = v // --output=FILE, CC=/tmp/x
		case strings.HasPrefix(a, "-"):
			if strings.ContainsAny(a, "/~") {
				return true // a value written onto its option: -f/tmp/x
			}
			continue
		}
		if outsideFolder(a, glob, cwd, root) {
			return true
		}
	}
	return false
}

// shellJoin writes argv back as a command line that reads the same way:
// a word with spaces or shell characters is single-quoted, unless the shell
// was meant to expand it (globs). `rm 'my file'` and `rm my file` stay two
// different commands.
func shellJoin(argv []string, globs []bool) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		special := " \t\n'\"\\$`;&|<>()#!"
		if i >= len(globs) || !globs[i] {
			special += "*?[]{}~"
		}
		if a == "" || strings.ContainsAny(a, special) {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// stepKey is the grant one simple command needs: its key, or the exact
// command when a key would cover more than the command shows.
func stepKey(argv []string, globs []bool, vars bool, cwd, root string) string {
	exact := shellJoin(argv, globs)
	if vars {
		return exact // a $VALUE is only known when it runs
	}
	key, ok := sessionKey(argv, cwd, root)
	if !ok || widens(argv, globs, key, cwd, root) {
		return exact
	}
	return key
}

// plainCommand reads a line that is one simple command with no redirection
// or substitution, after plain wrappers such as timeout. ok is false for
// everything else, including sudo, env and NAME=value prefixes.
func plainCommand(command string) (argv []string, globs []bool, vars, ok bool) {
	line := policy.ParseShell(command)
	if len(line.Commands) != 1 || len(line.Subs) > 0 || len(line.Commands[0].Redirects) > 0 {
		return nil, nil, false, false
	}
	c := line.Commands[0]
	argv, seen := policy.UnwrapCommand(c.Words)
	if len(argv) == 0 || !plainPrefix(c.Words, argv, seen) {
		return nil, nil, false, false
	}
	return argv, argvGlobs(c, argv), c.Vars, true
}

// argvGlobs are c.Globs for argv, the words left after the wrappers.
func argvGlobs(c policy.ShellCommand, argv []string) []bool {
	if n := len(c.Words) - len(argv); n >= 0 && len(c.Globs) == len(c.Words) {
		return c.Globs[n:]
	}
	return nil
}

// grantPrefix is what an "allow for this session" covers for a line
// grantSteps finds no part in: for shell tools the command key, or the
// exact command when it has none; other tools: the whole tool ("").
func grantPrefix(req api.Request, root string) string {
	if req.ToolKind != api.ToolShell {
		return ""
	}
	if argv, globs, vars, ok := plainCommand(req.Command); ok && !strings.ContainsAny(req.Command, shellControl) {
		return stepKey(argv, globs, vars, req.Cwd, root)
	}
	return strings.Join(strings.Fields(req.Command), " ")
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
	if g, ok := matchWhole(grants, tool, kind, command, cwd, root); ok {
		return g, true
	}
	if kind != api.ToolShell {
		return api.Grant{}, false
	}
	return matchSteps(grants, tool, command, cwd, root)
}

// matchWhole matches a call as one unit: a plain command, never a chain.
// It matches by key only when the command doesn't widen it (stepKey), so a
// "git push" grant never covers `git push --force`.
func matchWhole(grants []api.Grant, tool string, kind api.ToolKind, command, cwd, root string) (api.Grant, bool) {
	var exact, key string
	plain := kind == api.ToolShell && !strings.ContainsAny(command, shellControl)
	if plain {
		exact = strings.Join(strings.Fields(command), " ")
		if argv, globs, vars, ok := plainCommand(command); ok {
			key = stepKey(argv, globs, vars, cwd, root)
		}
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
		if !plain {
			continue
		}
		prefix := strings.Join(strings.Fields(g.Prefix), " ")
		if key == prefix || exact == prefix {
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
		globs := argvGlobs(c, argv)
		switch {
		case prog == "cd":
			if len(argv) != 2 || c.Vars || len(globs) != 2 || globs[1] || !insideRoot(argv[1], cwd, root) {
				ok = false
				continue
			}
			cwd = resolveIn(argv[1], cwd)
		case neutralTools[prog]:
		case readTools[prog] && !c.Vars && readerSafe(prog, argv[1:]) && allInside(argv[1:], globs, cwd, root):
			// At the end of a pipe (`| head -20`) it only shapes output.
			if !c.PipedIn {
				steps = append(steps, grantStep{key: readGrant, reader: true})
			}
		case prog == "ls" && !c.Vars && allInside(argv[1:], globs, cwd, root):
		default:
			steps = append(steps, grantStep{key: stepKey(argv, globs, c.Vars, cwd, root)})
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
			if (r.Glob && expandsElsewhere(r.Target)) || !insideRoot(r.Target, cwd, root) {
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
// something. sed and sort name the options they may take; the rest name the
// ones they may not.
func readerSafe(prog string, args []string) bool {
	var pos []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
		}
	}
	switch prog {
	case "sed":
		return sedPrintsOnly(args)
	case "sort":
		return sortReadsOnly(args)
	case "find":
		for _, a := range args {
			switch a {
			case "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprint0", "-fprintf", "-fls":
				return false
			}
		}
	case "grep", "egrep", "fgrep":
		return !attachedFile(args, "ABCDdem")
	case "rg":
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") || strings.HasPrefix(a, "--hostname-bin") {
				return false
			}
		}
		return !attachedFile(args, "ABCEMTdegjmrt")
	case "uniq":
		return len(pos) <= 1 // a second name is its output file
	}
	return true
}

// sedPrintsOnly allows `sed -n` with print scripts and nothing else: -i
// edits in place, -f reads a script from a file, and a script can write
// (w) or run (e) things.
func sedPrintsOnly(args []string) bool {
	quiet, script := false, false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-e" || a == "--expression":
			i++
			if i >= len(args) || !sedPrint.MatchString(args[i]) {
				return false
			}
			script = true
		case a == "--quiet" || a == "--silent":
			quiet = true
		case a == "--regexp-extended" || a == "--posix" || a == "--unbuffered":
		case strings.HasPrefix(a, "--"):
			return false
		case strings.HasPrefix(a, "-") && a != "-":
			if strings.Trim(a[1:], "nErsuz") != "" {
				return false
			}
			quiet = quiet || strings.Contains(a, "n")
		case !script:
			if !sedPrint.MatchString(a) {
				return false
			}
			script = true
		}
	}
	return quiet && script
}

var sortLongFlags = flagSet("--reverse", "--numeric-sort", "--unique", "--ignore-case", "--human-numeric-sort",
	"--version-sort", "--general-numeric-sort", "--month-sort", "--random-sort", "--stable", "--check",
	"--ignore-leading-blanks", "--dictionary-order", "--ignore-nonprinting", "--zero-terminated", "--merge",
	"--key", "--field-separator", "--sort")

// sortReadsOnly allows sort's ordering options. -o, --output (and its
// abbreviations) write a file, -T picks where temporary files go and
// --compress-program runs one; an option not listed here asks.
func sortReadsOnly(args []string) bool {
	for _, a := range args {
		switch {
		case a == "-" || !strings.HasPrefix(a, "-"):
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(a, "=")
			if !sortLongFlags[name] {
				return false
			}
		default:
			for _, ch := range a[1:] {
				if strings.ContainsRune("ktS", ch) {
					break // the rest of the word is its value
				}
				if !strings.ContainsRune("bdfghiMnRrsuVzcCm", ch) {
					return false
				}
			}
		}
	}
	return true
}

// attachedFile reports a pattern file written onto its option (-f.env,
// -rf/etc/passwd): allInside can't see where the name starts. valued are the
// other short options that take a value, which ends the cluster.
func attachedFile(args []string, valued string) bool {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || strings.HasPrefix(a, "--") {
			continue
		}
		for i, ch := range a[1:] {
			if ch == 'f' && i+2 < len(a) {
				return true
			}
			if ch == 'f' || strings.ContainsRune(valued, ch) {
				break
			}
		}
	}
	return false
}

// allInside checks every word that could be a path. A search pattern that
// looks like one outside the folder only costs a question. globs is
// parallel to the command's words: globs[i+1] belongs to args[i].
func allInside(args []string, globs []bool, cwd, root string) bool {
	for i, a := range args {
		glob := i+1 < len(globs) && globs[i+1]
		if strings.HasPrefix(a, "-") && a != "-" {
			_, v, has := strings.Cut(a, "=")
			if !has {
				if strings.ContainsAny(a, "/~") {
					return false // a value written onto its option: -f/etc/passwd
				}
				continue
			}
			a = v
		}
		if (glob && expandsElsewhere(a)) || !insideRoot(a, cwd, root) {
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
		return []string{grantPrefix(req, root)}
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

package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"
	"cel.dev/cel-go/interpreter"

	"github.com/sabr-software/pilothouse/internal/paths"
)

// celCostLimit bounds a single `when:` evaluation so a pathological
// expression (nested comprehensions over a huge input) cannot stall the
// pre_tool checkpoint. Ordinary rules cost well under 1000.
const celCostLimit = 100_000

var (
	celEnvOnce sync.Once
	celEnv     *cel.Env
	celEnvErr  error
)

// sharedCELEnv builds the environment once: NewEnv is expensive (ms) and
// the declarations never change.
func sharedCELEnv() (*cel.Env, error) {
	celEnvOnce.Do(func() {
		str := cel.StringType
		celEnv, celEnvErr = cel.NewEnv(
			cel.Variable("agent", str),
			cel.Variable("tool", str),
			cel.Variable("kind", str),
			cel.Variable("command", str),
			cel.Variable("paths", cel.ListType(str)),
			cel.Variable("path", str),
			cel.Variable("input", cel.MapType(str, cel.DynType)),
			cel.Variable("cwd", str),
			cel.Variable("repo", str),
			cel.Variable("branch", str),
			cel.Variable("mode", str),
			cel.Variable("checkpoint", str),
			cel.Variable("hour", cel.IntType),
			cel.Variable("weekday", cel.IntType),
			ext.Strings(),
			cel.Function("glob",
				cel.Overload("glob_string_string", []*cel.Type{str, str}, cel.BoolType,
					cel.BinaryBinding(celGlob)),
				cel.MemberOverload("string_glob_string", []*cel.Type{str, str}, cel.BoolType,
					cel.BinaryBinding(celGlob)),
			),
			cel.Function("has_word",
				cel.Overload("has_word_string_string", []*cel.Type{str, str}, cel.BoolType,
					cel.BinaryBinding(celHasWord)),
				cel.MemberOverload("string_has_word_string", []*cel.Type{str, str}, cel.BoolType,
					cel.BinaryBinding(celHasWord)),
			),
		)
	})
	return celEnv, celEnvErr
}

// compileWhen type-checks a `when:` expression. It must produce a bool
// (or dyn, e.g. `input.flag`, checked at run time).
func compileWhen(src string) (cel.Program, error) {
	env, err := sharedCELEnv()
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(src)
	if iss != nil && iss.Err() != nil {
		var msgs []string
		for _, e := range iss.Errors() {
			loc := e.Location
			if loc != nil && loc.Line() > 0 {
				msgs = append(msgs, fmt.Sprintf("%s (expression line %d, column %d)", e.Message, loc.Line(), loc.Column()+1))
			} else {
				msgs = append(msgs, e.Message)
			}
		}
		return nil, fmt.Errorf("%s", strings.Join(msgs, "; "))
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return nil, fmt.Errorf("must evaluate to a bool, not %s", t)
	}
	return env.Program(ast,
		cel.CostLimit(celCostLimit),
		cel.EvalOptions(cel.OptOptimize),
	)
}

// celGlobCache holds patterns used by glob() in `when:` expressions.
// They are almost always constants, so caching makes glob() as cheap as
// a match-block glob. Bounded so dynamic patterns can't grow it forever.
var celGlobCache struct {
	sync.RWMutex
	home string // patterns were expanded against this home; reset if it changes
	m    map[string]*glob
}

const celGlobCacheMax = 1024

func cachedPathGlob(pattern string) (*glob, error) {
	home := paths.UserHome()
	celGlobCache.RLock()
	var g *glob
	if celGlobCache.home == home {
		g = celGlobCache.m[pattern]
	}
	celGlobCache.RUnlock()
	if g != nil {
		return g, nil
	}
	g, err := compileGlob(expandHome(pattern, home), globPath)
	if err != nil {
		return nil, err
	}
	celGlobCache.Lock()
	if celGlobCache.m == nil || celGlobCache.home != home || len(celGlobCache.m) >= celGlobCacheMax {
		celGlobCache.m, celGlobCache.home = make(map[string]*glob), home
	}
	celGlobCache.m[pattern] = g
	celGlobCache.Unlock()
	return g, nil
}

// celGlob implements glob(s, pattern) with path semantics: '*' and '?'
// stop at '/', '**' crosses it, a leading "~/" is the user's home.
func celGlob(lhs, rhs ref.Val) ref.Val {
	s, ok1 := lhs.(types.String)
	p, ok2 := rhs.(types.String)
	if !ok1 || !ok2 {
		return types.MaybeNoSuchOverloadErr(lhs)
	}
	g, err := cachedPathGlob(string(p))
	if err != nil {
		return types.NewErr("glob(%q): %v", string(p), err)
	}
	return types.Bool(g.match(string(s)))
}

// celHasWord implements has_word(s, word).
func celHasWord(lhs, rhs ref.Val) ref.Val {
	s, ok1 := lhs.(types.String)
	w, ok2 := rhs.(types.String)
	if !ok1 || !ok2 {
		return types.MaybeNoSuchOverloadErr(lhs)
	}
	return types.Bool(hasWord(string(s), string(w)))
}

// hasWord reports whether word occurs in s as a whole word. Words are
// separated by whitespace and shell punctuation (; & | ( ) < > ' " ` $ =
// , and :), so has_word("git push --force-with-lease", "--force") is
// false while has_word("x=1;rm -rf /", "rm") and
// has_word("git push origin HEAD:main", "main") are true. '-', '.', '/'
// and '_' are part of words.
func hasWord(s, word string) bool {
	if word == "" {
		return false
	}
	for i := 0; i <= len(s)-len(word); {
		j := strings.Index(s[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		if (start == 0 || isWordSep(s[start-1])) && (end == len(s) || isWordSep(s[end])) {
			return true
		}
		i = start + 1
	}
	return false
}

func isWordSep(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f', ';', '&', '|', '(', ')', '<', '>', '\'', '"', '`', '$', '=', ',', ':':
		return true
	}
	return false
}

// activation resolves CEL variables lazily from the evaluation context:
// most rules never touch `input`, so its JSON is decoded at most once
// and only when an expression reads it.
type activation struct{ c *evalCtx }

var _ interpreter.Activation = activation{}

func (a activation) Parent() interpreter.Activation { return nil }

func (a activation) ResolveName(name string) (any, bool) {
	c := a.c
	switch name {
	case "agent":
		return types.String(c.in.Agent), true
	case "tool":
		return types.String(c.in.Tool), true
	case "kind":
		return types.String(c.in.ToolKind), true
	case "command":
		return types.String(c.in.Command), true
	case "paths":
		return c.celPaths(), true
	case "path":
		if p := c.absPaths(); len(p) > 0 {
			return types.String(p[0]), true
		}
		return types.String(""), true
	case "input":
		return c.celInput(), true
	case "cwd":
		return types.String(c.cwd), true
	case "repo":
		return types.String(c.repo), true
	case "branch":
		return types.String(c.in.Branch), true
	case "mode":
		return types.String(c.in.Mode), true
	case "checkpoint":
		return types.String(c.in.Checkpoint), true
	case "hour":
		return types.Int(c.now.Hour()), true
	case "weekday":
		return types.Int(c.now.Weekday()), true
	}
	return nil, false
}

// decodeInput turns the raw tool input into a map for CEL. Anything that
// isn't a JSON object yields an empty map so `has(input.x)` stays safe.
func decodeInput(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
		if m == nil {
			m = map[string]any{}
		}
	}
	return m
}

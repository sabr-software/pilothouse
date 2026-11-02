package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sabr-software/pilothouse/internal/api"
	"github.com/sabr-software/pilothouse/internal/client"
	"github.com/sabr-software/pilothouse/internal/config"
)

func rmtIsolate(t *testing.T) {
	t.Helper()
	t.Setenv("PILOTHOUSE_HOME", t.TempDir())
	t.Setenv("PILOTHOUSE_USER_HOME", t.TempDir())
	run, err := os.MkdirTemp("/tmp", "ag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(run) })
	t.Setenv("PILOTHOUSE_RUN_DIR", run)
	oldClient, oldOpen, oldTS, oldNow := rmtClient, rmtOpenBrowser, rmtTailscale, rmtNow
	t.Cleanup(func() { rmtClient, rmtOpenBrowser, rmtTailscale, rmtNow = oldClient, oldOpen, oldTS, oldNow })
}

type rmtFakeDaemon struct {
	pair    api.PairStart
	err     error
	devices []api.Device
	revoked []string
	events  []api.Event
}

func (f *rmtFakeDaemon) Pair(context.Context) (api.PairStart, error) { return f.pair, f.err }
func (f *rmtFakeDaemon) LoginLink(context.Context) (api.LoginLink, error) {
	return api.LoginLink{URL: "http://127.0.0.1:7517/login#AAAA-BBBB-CCCC"}, f.err
}
func (f *rmtFakeDaemon) Devices(context.Context) ([]api.Device, error) { return f.devices, f.err }
func (f *rmtFakeDaemon) RevokeDevice(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return f.err
}
func (f *rmtFakeDaemon) Snapshot(context.Context) (api.Snapshot, error) {
	return api.Snapshot{Seq: 10}, f.err
}
func (f *rmtFakeDaemon) Stream(ctx context.Context, after int64, fn func(api.Event)) error {
	for _, e := range f.events {
		if e.Seq > after {
			fn(e)
		}
	}
	<-ctx.Done()
	return nil
}

func rmtRun(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

func TestPairCommand(t *testing.T) {
	rmtIsolate(t)
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	rmtNow = func() time.Time { return now }
	f := &rmtFakeDaemon{pair: api.PairStart{Code: "K7QF-M2XA-P9TT", URL: "https://mac.tail1.ts.net/pair#K7QF-M2XA-P9TT", ExpiresAt: now.Add(10 * time.Minute), Remote: true}}
	rmtClient = func() rmtDaemon { return f }

	out, err := rmtRun(t, newPairCmd(), "--no-wait")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Scan with your phone", "▀", "https://mac.tail1.ts.net/pair#K7QF-M2XA-P9TT", "K7QF-M2XA-P9TT", "expires in 10 min"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No remote URL") {
		t.Error("remote warning shown with a remote URL")
	}

	// Local only: a clear warning, no QR.
	f.pair.Remote, f.pair.URL = false, "http://127.0.0.1:7517/pair#K7QF-M2XA-P9TT"
	out, _ = rmtRun(t, newPairCmd(), "--no-wait")
	if !strings.Contains(out, "pilothouse remote tailscale") || strings.Contains(out, "▀") {
		t.Errorf("local-only output:\n%s", out)
	}

	// Waits for the device to appear.
	d, _ := json.Marshal(api.Device{ID: "d_abcdefghij", Name: "Ada's iPhone", CreatedAt: now.Add(time.Minute)})
	old, _ := json.Marshal(api.Device{ID: "d_existing01", Name: "Old phone"})
	f.devices = []api.Device{{ID: "d_existing01", Name: "Old phone"}}
	f.events = []api.Event{{Seq: 11, Type: api.EventDeviceChanged, Data: old}, {Seq: 12, Type: api.EventDeviceChanged, Data: d}}
	f.pair.ExpiresAt = time.Now().Add(10 * time.Minute)
	out, err = rmtRun(t, newPairCmd(), "--no-qr")
	if err != nil || !strings.Contains(out, `Paired "Ada's iPhone"`) {
		t.Fatalf("wait: %v\n%s", err, out)
	}

	out, _ = rmtRun(t, newPairCmd(), "--json")
	var p api.PairStart
	if json.Unmarshal([]byte(out), &p) != nil || p.Code != "K7QF-M2XA-P9TT" {
		t.Fatalf("json: %s", out)
	}

	f.err = &client.APIError{Status: 403, Code: "agent_caller", Msg: "forbidden"}
	if _, err := rmtRun(t, newPairCmd()); err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Fatalf("agent caller: %v", err)
	}
}

func TestDevicesCommand(t *testing.T) {
	rmtIsolate(t)
	now := time.Now()
	seen := now.Add(-5 * time.Minute)
	f := &rmtFakeDaemon{devices: []api.Device{
		{ID: "d_aaaa111111", Name: "iPhone", PushOK: true, CreatedAt: now, LastSeenAt: &seen},
		{ID: "d_aaaa222222", Name: "iPad", CreatedAt: now},
	}}
	rmtClient = func() rmtDaemon { return f }
	out, err := rmtRun(t, newDevicesCmd())
	if err != nil || !strings.Contains(out, "d_aaaa111111") || !strings.Contains(out, "5m ago") || !strings.Contains(out, "never") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if _, err := rmtRun(t, newDevicesCmd(), "revoke", "d_aaaa"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := rmtRun(t, newDevicesCmd(), "revoke", "d_zzz"); err == nil {
		t.Fatal("unknown device revoked")
	}
	out, err = rmtRun(t, newDevicesCmd(), "revoke", "d_aaaa2")
	if err != nil || len(f.revoked) != 1 || f.revoked[0] != "d_aaaa222222" || !strings.Contains(out, `Revoked "iPad"`) {
		t.Fatalf("revoke: %v %v\n%s", err, f.revoked, out)
	}
	f.devices = nil
	if out, _ := rmtRun(t, newDevicesCmd()); !strings.Contains(out, "pilothouse pair") {
		t.Fatalf("empty: %s", out)
	}
}

func TestOpenCommand(t *testing.T) {
	rmtIsolate(t)
	rmtClient = func() rmtDaemon { return &rmtFakeDaemon{} }
	var opened string
	rmtOpenBrowser = func(u string) error { opened = u; return nil }
	if _, err := rmtRun(t, newOpenCmd()); err != nil || opened != "http://127.0.0.1:7517/login#AAAA-BBBB-CCCC" {
		t.Fatalf("open: %v %q", err, opened)
	}
	opened = ""
	out, _ := rmtRun(t, newOpenCmd(), "--print")
	if opened != "" || strings.TrimSpace(out) != "http://127.0.0.1:7517/login#AAAA-BBBB-CCCC" {
		t.Fatalf("print: %q", out)
	}
	rmtOpenBrowser = func(string) error { return errors.New("no display") }
	if out, _ := rmtRun(t, newOpenCmd()); !strings.Contains(out, "Open this one-time link yourself") {
		t.Fatalf("fallback: %s", out)
	}
	rmtClient = func() rmtDaemon { return &rmtFakeDaemon{err: client.ErrDaemonDown} }
	if _, err := rmtRun(t, newOpenCmd()); !errors.Is(err, client.ErrDaemonDown) {
		t.Fatalf("daemon down: %v", err)
	}
}

type rmtFakeTS struct {
	installed bool // on PATH
	macApp    bool
	status    string
	calls     [][]string
	serveErr  error
}

func (f *rmtFakeTS) exec() rmtTSExec {
	return rmtTSExec{
		lookPath: func(string) (string, error) {
			if f.installed {
				return "/usr/local/bin/tailscale", nil
			}
			return "", errors.New("not found")
		},
		stat: func(p string) (os.FileInfo, error) {
			if f.macApp && p == rmtMacTailscale {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		},
		output: func(_ context.Context, name string, args ...string) ([]byte, error) {
			f.calls = append(f.calls, append([]string{name}, args...))
			if len(args) > 0 && args[0] == "status" {
				return []byte(f.status), nil
			}
			return []byte("https://mac.tail1.ts.net (tailnet only)\n|-- / proxy http://127.0.0.1:7600\n"), nil
		},
		interactive: func(_ context.Context, name string, args ...string) error {
			f.calls = append(f.calls, append([]string{name}, args...))
			return f.serveErr
		},
	}
}

const rmtGoodStatus = `{"BackendState":"Running","CertDomains":["mac.tail1.ts.net"],
 "Self":{"DNSName":"mac.tail1.ts.net.","Online":true},"CurrentTailnet":{"Name":"me@example.com","MagicDNSEnabled":true}}`

func TestRemoteTailscale(t *testing.T) {
	rmtIsolate(t)
	if got := strings.Join(rmtServeArgs("7517"), " "); got != "serve --bg --https=443 http://127.0.0.1:7517" {
		t.Fatal(got)
	}
	if got := strings.Join(rmtServeOffArgs(), " "); got != "serve --https=443 off" {
		t.Fatal(got)
	}

	cfg := config.Default()
	cfg.Web.Addr = "127.0.0.1:7600"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	f := &rmtFakeTS{macApp: true, status: rmtGoodStatus}
	rmtTailscale = f.exec()
	out, err := rmtRun(t, newRemoteCmd(), "tailscale")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(f.calls) != 2 || strings.Join(f.calls[0], " ") != rmtMacTailscale+" status --json" ||
		strings.Join(f.calls[1], " ") != rmtMacTailscale+" serve --bg --https=443 http://127.0.0.1:7600" {
		t.Fatalf("calls = %q", f.calls)
	}
	got, _ := config.Load()
	if got.Web.PublicURL != "https://mac.tail1.ts.net" || got.Web.Addr != "127.0.0.1:7600" {
		t.Fatalf("config = %+v", got.Web)
	}
	if !strings.Contains(out, "pilothouse pair") {
		t.Errorf("no next step:\n%s", out)
	}

	out, err = rmtRun(t, newRemoteCmd(), "status")
	if err != nil || !strings.Contains(out, "Remote:  https://mac.tail1.ts.net") || strings.Contains(out, "isn't proxying") {
		t.Fatalf("status: %v\n%s", err, out)
	}

	f.calls = nil
	out, err = rmtRun(t, newRemoteCmd(), "off")
	if err != nil || len(f.calls) != 1 || strings.Join(f.calls[0][1:], " ") != "serve --https=443 off" {
		t.Fatalf("off: %v %q\n%s", err, f.calls, out)
	}
	if got, _ := config.Load(); got.Web.PublicURL != "" {
		t.Fatal("public_url not cleared")
	}
}

func TestRemoteTailscaleProblems(t *testing.T) {
	rmtIsolate(t)
	for _, tc := range []struct {
		name string
		ts   rmtFakeTS
		want string
	}{
		{"not installed", rmtFakeTS{}, "tailscale.com/download"},
		{"stopped", rmtFakeTS{installed: true, status: `{"BackendState":"Stopped"}`}, "tailscale up"},
		{"no magicdns", rmtFakeTS{installed: true, status: `{"BackendState":"Running","Self":{"DNSName":""}}`}, "MagicDNS"},
		{"no https", rmtFakeTS{installed: true, status: `{"BackendState":"Running","Self":{"DNSName":"mac.tail1.ts.net."},"CurrentTailnet":{"MagicDNSEnabled":true}}`}, "Enable HTTPS"},
		{"bad json", rmtFakeTS{installed: true, status: `nope`}, "tailscale status"},
		{"serve fails", rmtFakeTS{installed: true, status: rmtGoodStatus, serveErr: errors.New("exit 1")}, "enable Serve"},
	} {
		f := tc.ts
		rmtTailscale = f.exec()
		_, err := rmtRun(t, newRemoteCmd(), "tailscale")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if got, _ := config.Load(); got.Web.PublicURL != "" {
			t.Errorf("%s: public_url saved on failure", tc.name)
		}
	}
}

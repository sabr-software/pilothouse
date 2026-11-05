package web

import (
	"net"
	"net/http"
	"testing"
)

const (
	uaChrome  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"
	uaEdge    = uaChrome + " Edg/141.0.0.0"
	uaOpera   = uaChrome + " OPR/122.0.0.0"
	uaFirefox = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:144.0) Gecko/20100101 Firefox/144.0"
	uaSafari  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Safari/605.1.15"
	uaIPhone  = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1"
)

func TestBrowserBundle(t *testing.T) {
	for _, c := range []struct {
		name, ua, ch, want string
	}{
		{"Chrome", uaChrome, `"Google Chrome";v="141", "Not?A_Brand";v="8", "Chromium";v="141"`, "com.google.Chrome"},
		{"Edge", uaEdge, `"Microsoft Edge";v="141", "Not?A_Brand";v="8", "Chromium";v="141"`, "com.microsoft.edgemac"},
		{"Brave", uaChrome, `"Brave";v="141", "Not?A_Brand";v="8", "Chromium";v="141"`, "com.brave.Browser"},
		{"Opera", uaOpera, `"Opera";v="122", "Not?A_Brand";v="8", "Chromium";v="137"`, "com.operasoftware.Opera"},
		{"Opera without its brand", uaOpera, `"Not?A_Brand";v="8", "Chromium";v="137"`, "com.operasoftware.Opera"},
		{"Chromium that won't say (Arc, Vivaldi)", uaChrome, `"Chromium";v="141", "Not?A_Brand";v="8"`, ""},
		{"Chrome without client hints", uaChrome, "", "com.google.Chrome"},
		{"Firefox", uaFirefox, "", "org.mozilla.firefox"},
		{"Safari", uaSafari, "", "com.apple.Safari"},
		{"iPhone", uaIPhone, "", ""},
		{"no User-Agent", "", "", ""},
	} {
		h := http.Header{}
		h.Set("User-Agent", c.ua)
		if c.ch != "" {
			h.Set("Sec-CH-UA", c.ch)
		}
		if got := browserBundle(h); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// The browser is learned from paired requests from this computer only
// (loopback, or Tailscale Serve relaying one of its own addresses), and
// kept across restarts.
func TestDesktopBrowserLearnedOnThisComputer(t *testing.T) {
	e := newEnv(t)
	old := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("100.64.0.7"), Mask: net.CIDRMask(32, 32)}}, nil
	}
	t.Cleanup(func() { interfaceAddrs = old })
	_, pub := genKey(t)
	cookie, _ := e.pair(pub)
	get := func(cookie, ua, ch, xff string) {
		t.Helper()
		h := map[string]string{"User-Agent": ua}
		if ch != "" {
			h["Sec-CH-UA"] = ch
		}
		if xff != "" {
			h["X-Forwarded-For"] = xff
		}
		e.do("GET", "/api/me", nil, reqOpt{cookie: cookie, header: h})
	}
	if got := e.srv.DesktopBrowser(); got != "" {
		t.Fatalf("before any request: %q", got)
	}
	get(cookie, uaEdge, `"Microsoft Edge";v="141", "Chromium";v="141"`, "100.64.0.9") // another tailnet machine
	if got := e.srv.DesktopBrowser(); got != "" {
		t.Fatalf("another machine's browser counted: %q", got)
	}
	get(cookie, uaEdge, `"Microsoft Edge";v="141", "Chromium";v="141"`, "100.64.0.7") // this one, through Tailscale Serve
	if got := e.srv.DesktopBrowser(); got != "com.microsoft.edgemac" {
		t.Fatalf("through Tailscale Serve: %q", got)
	}
	get(cookie, uaFirefox, "", "") // over loopback
	if got := e.srv.DesktopBrowser(); got != "org.mozilla.firefox" {
		t.Fatalf("over loopback: %q", got)
	}
	get("", uaSafari, "", "") // not paired: doesn't count
	if got := e.srv.DesktopBrowser(); got != "org.mozilla.firefox" {
		t.Fatalf("an unpaired browser counted: %q", got)
	}

	again, err := New(Deps{Core: e.fc, DB: e.db, Config: e.cfg, Now: e.clock.Now,
		LocalGuard: func(h http.Handler) http.Handler { return h }})
	if err != nil {
		t.Fatal(err)
	}
	if got := again.DesktopBrowser(); got != "org.mozilla.firefox" {
		t.Fatalf("after a restart: %q", got)
	}
}

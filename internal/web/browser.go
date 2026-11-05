package web

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The browser a person uses Pilothouse in on this computer, so a click on
// a desktop notification opens its page there and not in a default
// browser that was never paired. It is learned from paired requests that
// come from this computer: over loopback, or through Tailscale Serve from
// one of this computer's own addresses.

// deskBrowserKey is the web_settings row that keeps the browser across
// restarts.
const deskBrowserKey = "desktop_browser"

// deskBrowser is the macOS bundle id of that browser.
type deskBrowser struct {
	mu     sync.Mutex
	id     string
	loaded bool
	addrs  map[string]bool // this computer's addresses
	addrAt time.Time
}

// interfaceAddrs lists this computer's addresses (tests replace it).
var interfaceAddrs = net.InterfaceAddrs

// chBrands maps Sec-CH-UA brands to bundle ids, most specific first.
// Chromium browsers send it; one that names only "Chromium" (Arc,
// Vivaldi) is left to the default browser.
var chBrands = []struct{ brand, id string }{
	{"Microsoft Edge", "com.microsoft.edgemac"},
	{"Brave", "com.brave.Browser"},
	{"Opera GX", "com.operasoftware.OperaGX"},
	{"Opera", "com.operasoftware.Opera"},
	{"Vivaldi", "com.vivaldi.Vivaldi"},
	{"Arc", "company.thebrowser.Browser"},
	{"YaBrowser", "ru.yandex.desktop.yandex-browser"},
	{"Yandex", "ru.yandex.desktop.yandex-browser"},
	{"DuckDuckGo", "com.duckduckgo.macos.browser"},
	{"Google Chrome", "com.google.Chrome"},
}

// uaTokens maps User-Agent tokens to bundle ids, most specific first, for
// browsers that send no Sec-CH-UA.
var uaTokens = []struct{ token, id string }{
	{"Edg/", "com.microsoft.edgemac"},
	{"OPR/", "com.operasoftware.Opera"},
	{"Vivaldi/", "com.vivaldi.Vivaldi"},
	{"YaBrowser/", "ru.yandex.desktop.yandex-browser"},
	{"Firefox/", "org.mozilla.firefox"},
	{"Chrome/", "com.google.Chrome"},
}

var brandRE = regexp.MustCompile(`"([^"]{1,64})"\s*;\s*v=`)

// browserBundle names the Mac browser that sent h by bundle id, or "" when
// it can't tell (or it isn't a Mac).
func browserBundle(h http.Header) string {
	ua := h.Get("User-Agent")
	if !strings.Contains(ua, "Macintosh") {
		return ""
	}
	if ch := h.Get("Sec-CH-UA"); ch != "" {
		brands := map[string]bool{}
		for _, m := range brandRE.FindAllStringSubmatch(ch, 8) {
			brands[m[1]] = true
		}
		for _, b := range chBrands {
			if brands[b.brand] {
				return b.id
			}
		}
		for _, t := range uaTokens[:4] { // ones that still say so in the UA
			if strings.Contains(ua, t.token) {
				return t.id
			}
		}
		return ""
	}
	for _, t := range uaTokens {
		if strings.Contains(ua, t.token) {
			return t.id
		}
	}
	if strings.Contains(ua, "Safari/") && strings.Contains(ua, "Version/") {
		return "com.apple.Safari"
	}
	return ""
}

// noteBrowser remembers the browser of a paired request from this
// computer.
func (s *Server) noteBrowser(r *http.Request) {
	id := browserBundle(r.Header)
	if id == "" || !s.fromThisComputer(r) {
		return
	}
	b := &s.desk
	b.mu.Lock()
	changed := b.id != id
	b.id, b.loaded = id, true
	b.mu.Unlock()
	if !changed {
		return
	}
	if _, err := s.d.DB.ExecContext(context.WithoutCancel(r.Context()),
		`INSERT INTO web_settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		deskBrowserKey, id); err != nil {
		s.log.Warn("save desktop browser", "err", err)
	}
}

// fromThisComputer reports whether r came from this computer: a loopback
// peer, unless it is the Tailscale Serve proxy relaying another peer (its
// X-Forwarded-For then has to be one of this computer's addresses).
func (s *Server) fromThisComputer(r *http.Request) bool {
	ip := net.ParseIP(clientKey(r))
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || s.ownAddr(ip)
}

func (s *Server) ownAddr(ip net.IP) bool {
	b := &s.desk
	b.mu.Lock()
	defer b.mu.Unlock()
	if now := time.Now(); b.addrs == nil || now.Sub(b.addrAt) > time.Minute {
		b.addrs, b.addrAt = map[string]bool{}, now
		as, err := interfaceAddrs()
		if err != nil {
			s.log.Warn("list addresses", "err", err)
		}
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok {
				b.addrs[n.IP.String()] = true
			}
		}
	}
	return b.addrs[ip.String()]
}

// DesktopBrowser is the bundle id of the browser Pilothouse was last used
// in on this computer, or "" (not known: the default browser).
func (s *Server) DesktopBrowser() string {
	b := &s.desk
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.loaded {
		var v string
		err := s.d.DB.QueryRow(`SELECT value FROM web_settings WHERE key = ?`, deskBrowserKey).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("load desktop browser", "err", err)
			return ""
		}
		b.id, b.loaded = v, true
	}
	return b.id
}

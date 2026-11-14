package web

import (
	"fmt"
	"net/http/httptest"
	"testing"
)

// A proxy that appends to X-Forwarded-For passes on whatever the caller
// sent first. Only the last address is the proxy's own word, so that is
// the one that names the caller.
func TestClientKeyTrustsOnlyTheProxysEntry(t *testing.T) {
	tests := []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"direct", "203.0.113.5:4000", nil, "203.0.113.5"},
		{"direct peers cannot set it", "203.0.113.5:4000", []string{"100.64.0.9"}, "203.0.113.5"},
		{"loopback without a proxy", "127.0.0.1:4000", nil, "127.0.0.1"},
		{"proxy that replaces the header", "127.0.0.1:4000", []string{"100.64.0.9"}, "100.64.0.9"},
		{"proxy that appends", "127.0.0.1:4000", []string{"127.0.0.1, 100.64.0.9"}, "100.64.0.9"},
		{"proxy that adds a second header", "[::1]:4000", []string{"10.0.0.1", "100.64.0.9"}, "100.64.0.9"},
		{"spaces", "127.0.0.1:4000", []string{" 10.0.0.1 ,  100.64.0.9 "}, "100.64.0.9"},
		{"empty last entry", "127.0.0.1:4000", []string{"10.0.0.1, "}, "127.0.0.1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = tt.remote
		for _, v := range tt.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientKey(r); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Behind an appending proxy, a caller who invents a new first address for
// every guess still shares one bucket.
func TestPairRateLimitIgnoresForgedForwardedFor(t *testing.T) {
	e := newEnv(t)
	_, pub := genKey(t)
	for i := 0; i < 12; i++ {
		w := e.do("POST", "/api/pair", pairBody{Code: "AAAA-BBBB-CCCC", PublicKey: pub},
			reqOpt{header: map[string]string{"X-Forwarded-For": fmt.Sprintf("198.51.100.%d, 100.64.0.9", i)}})
		if w.Code == 429 {
			return
		}
	}
	t.Fatal("twelve wrong codes from one caller with forged addresses were never limited")
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A rebound DNS name presents a matching spoofed Origin and Host. Trusting
// r.Host would accept it; an explicit allowlist must not.
func TestOriginRejectsRebindingWhenHostSpoofed(t *testing.T) {
	setAllowedHosts("100.115.109.120", 8384, "corenode")
	t.Cleanup(func() { setAllowedHosts("", 0) })

	r := httptest.NewRequest("GET", "http://evil.com/api/data", nil)
	r.Host = "evil.com"
	r.Header.Set("Origin", "http://evil.com")
	if originAllowed(r) {
		t.Error("accepted a request whose Host and Origin were both attacker-controlled")
	}
}

func TestOriginAllowlistedHosts(t *testing.T) {
	setAllowedHosts("100.115.109.120", 8384, "corenode")
	t.Cleanup(func() { setAllowedHosts("", 0) })

	cases := []struct {
		host, origin string
		want         bool
	}{
		{"100.115.109.120:8384", "http://100.115.109.120:8384", true},
		{"corenode:8384", "http://corenode:8384", true},
		{"localhost:8384", "http://localhost:8384", true},
		{"127.0.0.1:8384", "", true},
		{"100.115.109.120:8384", "http://evil.com", false},
		{"100.115.109.120:9999", "http://100.115.109.120:9999", false},
		{"192.168.150.54:8384", "http://192.168.150.54:8384", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "http://"+c.host+"/api/data", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := originAllowed(r); got != c.want {
			t.Errorf("host=%q origin=%q -> %v, want %v", c.host, c.origin, got, c.want)
		}
	}
}

// With no allowlist configured (unit context before serve starts) only
// loopback answers, so a stray binding cannot be probed cross-origin.
func TestOriginDefaultsToLoopbackWhenUnconfigured(t *testing.T) {
	setAllowedHosts("", 0)
	for _, c := range []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8384", true},
		{"localhost:9999", true},
		{"100.115.109.120:8384", false},
		{"evil.com:8384", false},
	} {
		r := httptest.NewRequest("GET", "http://"+c.host+"/api/data", nil)
		r.Host = c.host
		if got := originAllowed(r); got != c.want {
			t.Errorf("unconfigured host=%q -> %v, want %v", c.host, got, c.want)
		}
	}
}

var _ = http.StatusOK

// A reverse proxy on 80/443 sends an Origin with no port; a bare allowlist
// entry must match both that and the direct host:port form.
func TestOriginAllowsProxyHostWithAndWithoutPort(t *testing.T) {
	setAllowedHosts("100.115.109.120", 8384, "periscope.lan")
	t.Cleanup(func() { setAllowedHosts("", 0) })

	for _, c := range []struct {
		host, origin string
		want         bool
	}{
		{"periscope.lan", "https://periscope.lan", true},
		{"periscope.lan:8384", "http://periscope.lan:8384", true},
		{"periscope.lan", "https://elsewhere.lan", false},
		{"other.lan", "https://other.lan", false},
	} {
		r := httptest.NewRequest("GET", "http://"+c.host+"/api/data", nil)
		r.Host = c.host
		r.Header.Set("Origin", c.origin)
		if got := originAllowed(r); got != c.want {
			t.Errorf("host=%q origin=%q -> %v, want %v", c.host, c.origin, got, c.want)
		}
	}
}

// CORS headers do not stop DNS rebinding: the attacker's page is same-origin
// with the rebound name, so the browser applies no CORS check at all. The
// request itself must be refused.
func TestHostGuardRejectsUnknownHost(t *testing.T) {
	setAllowedHosts("100.115.109.120", 8384)
	t.Cleanup(func() { setAllowedHosts("", 0) })

	h := hostGuardMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("secret"))
	}))

	for _, c := range []struct {
		host string
		want int
	}{
		{"100.115.109.120:8384", 200},
		{"localhost:8384", 200},
		{"evil.com", 403},
		{"evil.com:8384", 403},
	} {
		r := httptest.NewRequest("GET", "/api/data", nil)
		r.Host = c.host
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != c.want {
			t.Errorf("Host %q -> %d, want %d", c.host, rr.Code, c.want)
		}
	}
}

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(host, origin string) *http.Request {
	r := httptest.NewRequest("GET", "http://"+host+"/api/data", nil)
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name, host, origin string
		want               bool
	}{
		{"no origin", "100.115.109.120:8384", "", true},
		{"same origin tailscale", "100.115.109.120:8384", "http://100.115.109.120:8384", true},
		{"same origin lan", "192.168.150.54:8384", "http://192.168.150.54:8384", true},
		{"localhost", "localhost:8384", "http://localhost:8384", true},
		{"loopback ip", "127.0.0.1:8384", "http://127.0.0.1:8384", true},
		{"localhost while bound elsewhere", "100.115.109.120:8384", "http://localhost:8384", true},
		{"foreign host", "100.115.109.120:8384", "http://evil.com", false},
		{"foreign host same port", "100.115.109.120:8384", "http://evil.com:8384", false},
		{"different lan host", "100.115.109.120:8384", "http://192.168.150.99:8384", false},
	}
	for _, c := range cases {
		if got := originAllowed(req(c.host, c.origin)); got != c.want {
			t.Errorf("%s: originAllowed(host=%q, origin=%q) = %v, want %v", c.name, c.host, c.origin, got, c.want)
		}
	}
}

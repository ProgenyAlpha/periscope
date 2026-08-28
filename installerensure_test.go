package main

import (
	"strings"
	"testing"
)

// The SessionStart hook probes a hardcoded localhost while cmdServe's
// "already running?" check uses the configured host. With host set to a LAN
// address the probe always failed, the hook spawned `periscope serve`, and that
// process found the server on the configured host and exited immediately — so
// the auto-start hook never once did anything.
func TestLauncherScript_ProbesTheConfiguredHost(t *testing.T) {
	cfg := ServerConfig{Host: "100.115.109.120", Port: 8384}

	name, content := launcherScript(cfg, "/usr/local/bin/periscope", "linux")
	if name != "periscope-ensure.sh" {
		t.Fatalf("name = %q", name)
	}
	if !strings.Contains(content, "http://100.115.109.120:8384/api/health") {
		t.Fatalf("probe URL does not use the configured host:\n%s", content)
	}
	if strings.Contains(content, "http://localhost:8384") {
		t.Fatalf("probe still points at localhost despite a configured host:\n%s", content)
	}
}

func TestLauncherScript_WindowsProbesTheConfiguredHost(t *testing.T) {
	cfg := ServerConfig{Host: "100.115.109.120", Port: 9000}

	name, content := launcherScript(cfg, `C:\bin\periscope.exe`, "windows")
	if name != "periscope-ensure.ps1" {
		t.Fatalf("name = %q", name)
	}
	if !strings.Contains(content, "http://100.115.109.120:9000/api/health") {
		t.Fatalf("probe URL does not use the configured host:\n%s", content)
	}
	if !strings.Contains(content, `C:\bin\periscope.exe`) {
		t.Fatalf("binary path missing:\n%s", content)
	}
}

// A wildcard or empty bind address is not a dialable destination; the probe
// has to fall back to loopback.
func TestProbeHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "localhost"},
		{"0.0.0.0", "localhost"},
		{"::", "localhost"},
		{"[::]", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"localhost", "localhost"},
		{"100.115.109.120", "100.115.109.120"},
		{"::1", "[::1]"},
		{"[::1]", "[::1]"},
		{"fd00::1", "[fd00::1]"},
		{"  100.115.109.120  ", "100.115.109.120"},
	}
	for _, c := range cases {
		if got := probeHost(c.in); got != c.want {
			t.Errorf("probeHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLauncherScript_WildcardBindFallsBackToLoopback(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::"} {
		_, content := launcherScript(ServerConfig{Host: host, Port: 8384}, "/bin/periscope", "linux")
		if !strings.Contains(content, "http://localhost:8384/api/health") {
			t.Fatalf("host %q: want a loopback probe, got:\n%s", host, content)
		}
	}
}

func TestLauncherScript_DefaultsThePortWhenUnset(t *testing.T) {
	_, content := launcherScript(ServerConfig{Host: "localhost"}, "/bin/periscope", "linux")
	if !strings.Contains(content, ":8384/api/health") {
		t.Fatalf("want the 8384 default when port is unset, got:\n%s", content)
	}
}

func TestLauncherScript_IPv6HostIsBracketed(t *testing.T) {
	_, content := launcherScript(ServerConfig{Host: "fd7a::1", Port: 8384}, "/bin/periscope", "linux")
	if !strings.Contains(content, "http://[fd7a::1]:8384/api/health") {
		t.Fatalf("IPv6 literal must be bracketed in the URL, got:\n%s", content)
	}
}

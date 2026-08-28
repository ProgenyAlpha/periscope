package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Everything here runs against t.TempDir() with injected func values. No test
// sends a real push, shells out to a real notifier, or writes to ~/.periscope.

func failingReport() doctorReport {
	return newDoctorReport(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), fixedResults())
}

func healthyReport() doctorReport {
	return newDoctorReport(time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC),
		[]checkResult{{checkServer, ckOK, "ok", ""}})
}

type recordingNotifier struct {
	pushed, desktopped []string
	pushErr, deskErr   error
	subscribers        int
	subsErr            error
	stderr             bytes.Buffer
}

func (r *recordingNotifier) build(statusFile string) doctorNotifier {
	return doctorNotifier{
		StatusFile:  statusFile,
		Subscribers: func() (int, error) { return r.subscribers, r.subsErr },
		Push: func(title, body string) error {
			r.pushed = append(r.pushed, title+": "+body)
			return r.pushErr
		},
		Desktop: func(title, body string) error {
			r.desktopped = append(r.desktopped, title+": "+body)
			return r.deskErr
		},
		Stderr: &r.stderr,
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ── the durable record ──────────────────────────────────────────────────────

func TestNotifyWritesAStatusFileOnEveryRun(t *testing.T) {
	for name, rep := range map[string]doctorReport{"healthy": healthyReport(), "failing": failingReport()} {
		// Deliberately nested under a directory that does not exist yet: the
		// scheduled run may be the first thing to touch ~/.periscope.
		path := filepath.Join(t.TempDir(), "nested", "doctor-status.json")
		r := &recordingNotifier{}
		channels := notifyDoctorRun(r.build(path), rep)

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: status file not written: %v", name, err)
		}
		var decoded doctorReport
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s: status file is not valid JSON: %v\n%s", name, err, raw)
		}
		if decoded.ExitCode != rep.ExitCode || len(decoded.Checks) != len(rep.Checks) {
			t.Fatalf("%s: status file does not match the report: %+v", name, decoded)
		}
		if !has(channels, "status-file") {
			t.Fatalf("%s: channels = %v, want status-file", name, channels)
		}
	}
}

func TestStatusFileIsRewrittenNotAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doctor-status.json")
	r := &recordingNotifier{}
	notifyDoctorRun(r.build(path), failingReport())
	notifyDoctorRun(r.build(path), healthyReport())

	var decoded doctorReport
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("second write left the file unparseable: %v\n%s", err, raw)
	}
	if decoded.Status != "healthy" {
		t.Fatalf("status file still says %q after a healthy run", decoded.Status)
	}
}

// ── a healthy run must stay silent ──────────────────────────────────────────

func TestNotifyStaysSilentOnAHealthyRun(t *testing.T) {
	r := &recordingNotifier{subscribers: 3}
	channels := notifyDoctorRun(r.build(filepath.Join(t.TempDir(), "s.json")), healthyReport())

	if len(r.pushed) != 0 || len(r.desktopped) != 0 {
		t.Fatalf("a healthy run alerted the user: push=%v desktop=%v", r.pushed, r.desktopped)
	}
	if r.stderr.Len() != 0 {
		t.Fatalf("a healthy run wrote to stderr: %q", r.stderr.String())
	}
	if has(channels, "push") || has(channels, "desktop") || has(channels, "stderr") {
		t.Fatalf("channels = %v, want status-file only", channels)
	}
}

// ── escalation ladder ───────────────────────────────────────────────────────

func TestNotifyPushesWhenSomeoneIsSubscribed(t *testing.T) {
	r := &recordingNotifier{subscribers: 1}
	channels := notifyDoctorRun(r.build(filepath.Join(t.TempDir(), "s.json")), failingReport())

	if len(r.pushed) != 1 {
		t.Fatalf("push not sent: %v", r.pushed)
	}
	if len(r.desktopped) != 0 {
		t.Fatalf("desktop notifier fired even though push worked: %v", r.desktopped)
	}
	if !has(channels, "push") {
		t.Fatalf("channels = %v, want push", channels)
	}
}

func TestNotifyFallsBackToTheDesktopWhenNobodyIsSubscribed(t *testing.T) {
	r := &recordingNotifier{subscribers: 0}
	channels := notifyDoctorRun(r.build(filepath.Join(t.TempDir(), "s.json")), failingReport())

	if len(r.pushed) != 0 {
		t.Fatalf("push sent with zero subscribers: %v", r.pushed)
	}
	if len(r.desktopped) != 1 {
		t.Fatalf("desktop notifier did not fire: %v", r.desktopped)
	}
	if !has(channels, "desktop") {
		t.Fatalf("channels = %v, want desktop", channels)
	}
}

func TestNotifyFallsBackToTheDesktopWhenPushFails(t *testing.T) {
	r := &recordingNotifier{subscribers: 2, pushErr: errors.New("no network")}
	channels := notifyDoctorRun(r.build(filepath.Join(t.TempDir(), "s.json")), failingReport())

	if len(r.desktopped) != 1 {
		t.Fatalf("a failed push did not fall through to the desktop: %v", r.desktopped)
	}
	if has(channels, "push") {
		t.Fatalf("channels = %v claims a push that errored", channels)
	}
}

// stderr is the floor of the ladder: under systemd it lands in the journal and
// marks the unit failed, under cron it becomes the mail. It must be written
// even when every richer channel is broken, because a failure that only ever
// reached a log file is the trap that hid the original outage.
func TestNotifyAlwaysWritesStderrOnFailure(t *testing.T) {
	r := &recordingNotifier{
		subsErr: errors.New("database locked"),
		pushErr: errors.New("no network"),
		deskErr: errors.New("no display"),
	}
	channels := notifyDoctorRun(r.build(filepath.Join(t.TempDir(), "s.json")), failingReport())

	out := r.stderr.String()
	if out == "" {
		t.Fatal("nothing was written to stderr on a failing run")
	}
	if !strings.Contains(out, checkSidecars) {
		t.Fatalf("stderr does not name the failing check:\n%s", out)
	}
	if !has(channels, "stderr") {
		t.Fatalf("channels = %v, want stderr", channels)
	}
}

func TestNotifyToleratesAnUnwritableStatusFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the file should be: writing it can only fail.
	path := filepath.Join(dir, "doctor-status.json")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	r := &recordingNotifier{subscribers: 1}
	channels := notifyDoctorRun(r.build(path), failingReport())

	if has(channels, "status-file") {
		t.Fatalf("channels = %v claims a status file it could not write", channels)
	}
	if len(r.pushed) != 1 {
		t.Fatalf("an unwritable status file suppressed the alert: %v", r.pushed)
	}
}

// ── message content ─────────────────────────────────────────────────────────

func TestNotificationNamesTheFailingChecks(t *testing.T) {
	title, body := doctorNotificationText(failingReport())
	if !strings.Contains(strings.ToLower(title), "periscope") {
		t.Fatalf("title %q does not identify periscope", title)
	}
	if !strings.Contains(body, checkSidecars) {
		t.Fatalf("body does not name the failing check: %q", body)
	}
	if strings.Contains(body, checkHooks) {
		t.Fatalf("body names a passing check: %q", body)
	}
	if !strings.Contains(body, "no session sidecars") {
		t.Fatalf("body drops the detail that says what is wrong: %q", body)
	}
}

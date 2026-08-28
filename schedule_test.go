package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// HARD RULE for this file: no test may create a real systemd unit or touch the
// real crontab. Every path is a t.TempDir() and every external command is a
// func value recorded in memory. `periscope doctor` exists because a silent
// failure went unnoticed for five days; its own test suite must not be able to
// leave a timer behind on the machine that ran it.

type fakeSystemctl struct {
	calls [][]string
	fail  map[string]bool // first arg → return an error
}

func (f *fakeSystemctl) run(args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if len(args) > 0 && f.fail[args[0]] {
		return []byte("boom"), os.ErrInvalid
	}
	return []byte("enabled\n"), nil
}

func (f *fakeSystemctl) called(sub string) bool {
	for _, c := range f.calls {
		if strings.Join(c, " ") == sub || (len(c) > 0 && c[0] == sub) {
			return true
		}
	}
	return false
}

type fakeCrontab struct {
	content string
	missing bool // `crontab -l` on a user with no crontab
	writes  int
}

func (f *fakeCrontab) read() (string, error) {
	if f.missing {
		return "", os.ErrNotExist
	}
	return f.content, nil
}

func (f *fakeCrontab) write(s string) error {
	f.content, f.missing = s, false
	f.writes++
	return nil
}

func testScheduleEnv(t *testing.T) (scheduleEnv, *fakeSystemctl, *fakeCrontab) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin", "periscope")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	sc := &fakeSystemctl{fail: map[string]bool{}}
	cr := &fakeCrontab{missing: true}
	return scheduleEnv{
		Binary:       bin,
		Args:         scheduleDoctorArgs,
		Interval:     defaultScheduleInterval,
		UnitDir:      filepath.Join(root, "config", "systemd", "user"),
		Systemctl:    sc.run,
		CrontabRead:  cr.read,
		CrontabWrite: cr.write,
	}, sc, cr
}

func countLines(s, needle string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// ── systemd ─────────────────────────────────────────────────────────────────

func TestInstallScheduleWritesASystemdUserTimer(t *testing.T) {
	env, sc, _ := testScheduleEnv(t)

	res, err := installSchedule(env)
	if err != nil {
		t.Fatalf("installSchedule: %v", err)
	}
	if res.Backend != "systemd" {
		t.Fatalf("backend = %q, want systemd", res.Backend)
	}

	for _, name := range []string{scheduleServiceFile, scheduleTimerFile} {
		if _, err := os.Stat(filepath.Join(env.UnitDir, name)); err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
	}
	if !sc.called("daemon-reload") {
		t.Fatalf("systemctl --user daemon-reload was never run; calls=%v", sc.calls)
	}
	if !sc.called("enable") {
		t.Fatalf("the timer was never enabled; calls=%v", sc.calls)
	}
}

// The bug this guards: a bare `periscope` in a non-interactive unit resolves
// against a PATH that does not contain ~/.local/bin, so the scheduled check
// silently never runs — the exact class of failure it was installed to catch.
func TestInstalledSystemdUnitUsesTheAbsoluteBinaryPath(t *testing.T) {
	env, _, _ := testScheduleEnv(t)
	if _, err := installSchedule(env); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(env.UnitDir, scheduleServiceFile))
	if err != nil {
		t.Fatalf("read service: %v", err)
	}
	unit := string(raw)
	if !strings.Contains(unit, "ExecStart="+env.Binary+" ") {
		t.Fatalf("ExecStart does not use the absolute binary path %q:\n%s", env.Binary, unit)
	}
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			cmd := strings.TrimPrefix(line, "ExecStart=")
			if !filepath.IsAbs(strings.Fields(cmd)[0]) {
				t.Fatalf("ExecStart runs a bare name: %q", cmd)
			}
		}
	}
	if !strings.Contains(unit, "doctor") {
		t.Fatalf("the unit does not run doctor:\n%s", unit)
	}
}

func TestInstallScheduleTwiceLeavesExactlyOneTimer(t *testing.T) {
	env, _, _ := testScheduleEnv(t)

	first, err := installSchedule(env)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if first.Action != "installed" {
		t.Fatalf("first install action = %q, want installed", first.Action)
	}
	before, err := os.ReadFile(filepath.Join(env.UnitDir, scheduleTimerFile))
	if err != nil {
		t.Fatalf("read timer: %v", err)
	}

	second, err := installSchedule(env)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if second.Action != "unchanged" {
		t.Fatalf("second install action = %q, want unchanged", second.Action)
	}

	entries, err := os.ReadDir(env.UnitDir)
	if err != nil {
		t.Fatalf("read unit dir: %v", err)
	}
	var timers, services int
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".timer"):
			timers++
		case strings.HasSuffix(e.Name(), ".service"):
			services++
		}
	}
	if timers != 1 || services != 1 {
		t.Fatalf("after two installs: %d timers, %d services, want 1 and 1 (%v)", timers, services, entries)
	}
	after, _ := os.ReadFile(filepath.Join(env.UnitDir, scheduleTimerFile))
	if string(before) != string(after) {
		t.Fatalf("timer content drifted between identical installs")
	}
}

func TestRemoveScheduleDeletesTheSystemdTimer(t *testing.T) {
	env, sc, _ := testScheduleEnv(t)
	if _, err := installSchedule(env); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}

	removed, err := removeSchedule(env)
	if err != nil {
		t.Fatalf("removeSchedule: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("removeSchedule reported nothing removed")
	}
	for _, name := range []string{scheduleServiceFile, scheduleTimerFile} {
		if _, err := os.Stat(filepath.Join(env.UnitDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived removal (err=%v)", name, err)
		}
	}
	if !sc.called("disable") {
		t.Fatalf("the timer was never disabled before its unit files were deleted; calls=%v", sc.calls)
	}

	// Removing again is a no-op, not an error: uninstall must be re-runnable.
	again, err := removeSchedule(env)
	if err != nil {
		t.Fatalf("second removeSchedule: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second removeSchedule reported %v, want nothing", again)
	}
}

// ── cron fallback ───────────────────────────────────────────────────────────

func TestInstallScheduleFallsBackToCronWithoutSystemd(t *testing.T) {
	env, _, cr := testScheduleEnv(t)
	env.Systemctl = nil

	res, err := installSchedule(env)
	if err != nil {
		t.Fatalf("installSchedule: %v", err)
	}
	if res.Backend != "cron" {
		t.Fatalf("backend = %q, want cron", res.Backend)
	}
	if countLines(cr.content, scheduleCronMarker) != 1 {
		t.Fatalf("want exactly one marked crontab line, got:\n%s", cr.content)
	}
	if !strings.Contains(cr.content, env.Binary+" doctor") {
		t.Fatalf("crontab line does not run the absolute binary:\n%s", cr.content)
	}
	if _, err := os.Stat(env.UnitDir); !os.IsNotExist(err) {
		t.Fatalf("the cron path created a systemd unit directory")
	}
}

func TestInstallScheduleTwiceLeavesExactlyOneCrontabLine(t *testing.T) {
	env, _, cr := testScheduleEnv(t)
	env.Systemctl = nil
	cr.missing = false
	cr.content = "# a crontab that already had work in it\n0 3 * * * /usr/bin/backup\n"

	for i := 0; i < 3; i++ {
		if _, err := installSchedule(env); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	if got := countLines(cr.content, scheduleCronMarker); got != 1 {
		t.Fatalf("three installs produced %d marked lines:\n%s", got, cr.content)
	}
	if !strings.Contains(cr.content, "/usr/bin/backup") {
		t.Fatalf("installing the schedule ate an unrelated crontab line:\n%s", cr.content)
	}
	if !strings.HasSuffix(cr.content, "\n") {
		t.Fatalf("crontab must end in a newline or cron drops the last line:\n%q", cr.content)
	}
}

func TestRemoveScheduleDeletesOnlyTheCrontabLineItOwns(t *testing.T) {
	env, _, cr := testScheduleEnv(t)
	env.Systemctl = nil
	cr.missing = false
	cr.content = "0 3 * * * /usr/bin/backup\n"

	if _, err := installSchedule(env); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}
	removed, err := removeSchedule(env)
	if err != nil {
		t.Fatalf("removeSchedule: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("removeSchedule reported nothing removed")
	}
	if strings.Contains(cr.content, scheduleCronMarker) {
		t.Fatalf("the marked line survived removal:\n%s", cr.content)
	}
	if !strings.Contains(cr.content, "/usr/bin/backup") {
		t.Fatalf("removal ate an unrelated crontab line:\n%s", cr.content)
	}
}

// A machine that used cron before systemd (or vice versa) must not be left with
// a second, invisible copy of the check. Uninstall sweeps both backends.
func TestRemoveScheduleSweepsBothBackends(t *testing.T) {
	env, _, cr := testScheduleEnv(t)

	noSystemd := env
	noSystemd.Systemctl = nil
	if _, err := installSchedule(noSystemd); err != nil {
		t.Fatalf("cron install: %v", err)
	}
	if _, err := installSchedule(env); err != nil {
		t.Fatalf("systemd install: %v", err)
	}

	removed, err := removeSchedule(env)
	if err != nil {
		t.Fatalf("removeSchedule: %v", err)
	}
	if len(removed) < 2 {
		t.Fatalf("removeSchedule reported %v, want both backends", removed)
	}
	if strings.Contains(cr.content, scheduleCronMarker) {
		t.Fatalf("the crontab line survived a systemd-backed removal:\n%s", cr.content)
	}
	if _, err := os.Stat(filepath.Join(env.UnitDir, scheduleTimerFile)); !os.IsNotExist(err) {
		t.Fatal("the timer unit survived removal")
	}
}

// ── guards ──────────────────────────────────────────────────────────────────

func TestInstallScheduleRefusesARelativeBinary(t *testing.T) {
	env, _, _ := testScheduleEnv(t)
	env.Binary = "periscope"
	if _, err := installSchedule(env); err == nil {
		t.Fatal("installSchedule accepted a bare binary name; a non-interactive PATH will not resolve it")
	}
}

func TestInstallScheduleRejectsAnUnusableInterval(t *testing.T) {
	env, _, _ := testScheduleEnv(t)
	env.Interval = "every-so-often"
	if _, err := installSchedule(env); err == nil {
		t.Fatal("installSchedule accepted a nonsense interval")
	}
}

func TestScheduleStatusReportsWhatIsInstalled(t *testing.T) {
	env, _, _ := testScheduleEnv(t)

	installed, detail, err := scheduleStatus(env)
	if err != nil {
		t.Fatalf("scheduleStatus: %v", err)
	}
	if installed {
		t.Fatalf("scheduleStatus says installed on a clean machine: %s", detail)
	}

	if _, err := installSchedule(env); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}
	installed, detail, err = scheduleStatus(env)
	if err != nil {
		t.Fatalf("scheduleStatus: %v", err)
	}
	if !installed || !strings.Contains(detail, scheduleTimerFile) {
		t.Fatalf("scheduleStatus = %v %q after install", installed, detail)
	}
}

// ── init must never schedule by accident ────────────────────────────────────

// The whole point of the conservative default: `periscope init` is run by the
// install script and by `periscope serve` on first run. Neither may silently
// add a timer or a crontab line to a machine.
func TestPlainInitDoesNotInstallASchedule(t *testing.T) {
	// Through the real argument path, both interactively and not.
	for _, interactive := range []bool{false, true} {
		opts, err := initOptions(nil, interactive)
		if err != nil {
			t.Fatalf("initOptions: %v", err)
		}
		if interactive && opts.ConfirmSchedule == nil {
			t.Fatal("an interactive init never offers the choice")
		}
		if !interactive && opts.ConfirmSchedule != nil {
			t.Fatal("a non-interactive init would block on a prompt")
		}
		// The non-interactive case is the one install.sh and `serve` take.
		if !interactive && scheduleDecision(opts) {
			t.Fatal("a plain non-interactive `periscope init` would install a schedule")
		}
	}
	// `serve`'s first-run install cannot reach the machine's scheduler at all.
	if (installOptions{}).Scheduler != nil {
		t.Fatal("the zero installOptions carries a scheduler hook")
	}
	if opts, _ := initOptions([]string{"--schedule"}, false); !scheduleDecision(opts) {
		t.Fatal("--schedule did not reach the decision")
	}

	if scheduleDecision(installOptions{}) {
		t.Fatal("a plain `periscope init` would install a schedule")
	}
	if scheduleDecision(installOptions{ConfirmSchedule: func() bool { return false }}) {
		t.Fatal("declining the prompt still installed a schedule")
	}
	if scheduleDecision(installOptions{Schedule: true, NoSchedule: true}) {
		t.Fatal("--no-schedule did not beat --schedule")
	}
	if !scheduleDecision(installOptions{Schedule: true}) {
		t.Fatal("--schedule did not install a schedule")
	}
	if !scheduleDecision(installOptions{ConfirmSchedule: func() bool { return true }}) {
		t.Fatal("accepting the prompt did not install a schedule")
	}
}

func TestParseInitFlags(t *testing.T) {
	cases := []struct {
		args    []string
		want    installOptions
		wantErr bool
	}{
		{args: nil, want: installOptions{}},
		{args: []string{"--schedule"}, want: installOptions{Schedule: true}},
		{args: []string{"--no-schedule"}, want: installOptions{NoSchedule: true}},
		{args: []string{"--schedule", "--interval", "6h"}, want: installOptions{Schedule: true, Interval: "6h"}},
		{args: []string{"--interval"}, wantErr: true},
		{args: []string{"--interval", "wat"}, wantErr: true},
		{args: []string{"--what"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseInitFlags(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseInitFlags(%v) accepted bad input", tc.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseInitFlags(%v): %v", tc.args, err)
		}
		if got.Schedule != tc.want.Schedule || got.NoSchedule != tc.want.NoSchedule || got.Interval != tc.want.Interval {
			t.Fatalf("parseInitFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

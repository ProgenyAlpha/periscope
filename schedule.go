package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// schedule.go — make something actually run `periscope doctor`.
//
// doctor was written after a five-day silent telemetry outage and it does catch
// that outage. It caught nothing for the weeks afterwards, because nothing
// invoked it. A diagnostic nobody runs is a diagnostic that does not exist, so
// this installs a recurring invocation: a systemd user timer where there is a
// user manager to talk to, a crontab line where there is not.
//
// Three rules the implementation is built around:
//
//  1. ABSOLUTE PATHS. The scheduled command names the binary by full path. A
//     bare `periscope` is resolved against the PATH of a non-interactive
//     session, which on this machine does not include ~/.local/bin — that is
//     how the statusline broke. A health check that cannot start is worse than
//     none, because its silence reads as health.
//
//  2. IDEMPOTENT. Installing twice writes the same two unit files or replaces
//     the same one marked crontab line. Two timers firing the same check would
//     double every alert until someone muted both.
//
//  3. NEVER SILENT. Nothing here runs as a side effect of a plain `init`;
//     see scheduleDecision in installer.go.

const (
	scheduleServiceFile = "periscope-doctor.service"
	scheduleTimerFile   = "periscope-doctor.timer"

	// scheduleCronMarker terminates the managed crontab line and is what makes
	// the cron backend idempotent and removable: install strips every line
	// carrying it before appending exactly one, and remove strips them and
	// stops. Nothing else in the crontab is ever read or rewritten.
	scheduleCronMarker = "# periscope-doctor (managed by `periscope schedule`)"

	defaultScheduleInterval = "1h"
)

// scheduleDoctorArgs is the invocation the schedule runs.
//
//	--json    a stable envelope, so a wrapper or a log scraper can parse it
//	--quiet   silence on a healthy run, so cron sends no mail and the journal
//	          does not fill with "everything is fine"
//	--notify  escalate a failure to a human (notify.go)
var scheduleDoctorArgs = []string{"doctor", "--json", "--quiet", "--notify"}

// scheduleTiming maps one supported interval onto both backends at once, so a
// systemd install and a cron install of the same interval mean the same thing.
type scheduleTiming struct {
	systemd string // OnUnitActiveSec=
	cron    string // five-field cron expression
	label   string
}

// The minute is 17 rather than 0 on purpose: every other cron job on a machine
// fires on the hour, and doctor opens the database and the HTTP health endpoint.
var scheduleIntervals = map[string]scheduleTiming{
	"15m": {"15min", "*/15 * * * *", "every 15 minutes"},
	"30m": {"30min", "*/30 * * * *", "every 30 minutes"},
	"1h":  {"1h", "17 * * * *", "hourly"},
	"6h":  {"6h", "17 */6 * * *", "every 6 hours"},
	"12h": {"12h", "17 */12 * * *", "every 12 hours"},
	"24h": {"24h", "17 9 * * *", "daily at 09:17"},
}

var scheduleIntervalAliases = map[string]string{"1d": "24h", "daily": "24h", "hourly": "1h"}

func normalizeInterval(interval string) (string, scheduleTiming, error) {
	key := strings.ToLower(strings.TrimSpace(interval))
	if key == "" {
		key = defaultScheduleInterval
	}
	if alias, ok := scheduleIntervalAliases[key]; ok {
		key = alias
	}
	timing, ok := scheduleIntervals[key]
	if !ok {
		return "", scheduleTiming{}, fmt.Errorf("unsupported interval %q (use one of: %s)", interval, supportedIntervals())
	}
	return key, timing, nil
}

func supportedIntervals() string {
	// Fixed order; ranging a map here would make the error message flap.
	return "15m, 30m, 1h, 6h, 12h, 24h"
}

// scheduleEnv is everything the scheduler touches. Every path and every
// external command is a field so the tests run entirely inside t.TempDir() and
// cannot install a real timer or crontab entry on the machine running them.
type scheduleEnv struct {
	Binary   string   // MUST be absolute; see rule 1
	Args     []string // arguments appended to Binary
	Interval string
	UnitDir  string // ~/.config/systemd/user

	// Systemctl is nil when there is no user manager to talk to. That is the
	// signal to fall back to cron, and it is decided once, in systemctlUser().
	Systemctl func(args ...string) ([]byte, error)

	CrontabRead  func() (string, error)
	CrontabWrite func(string) error
}

type scheduleResult struct {
	Backend string // systemd | cron
	Action  string // installed | updated | unchanged
	Detail  string
}

// ── Install ─────────────────────────────────────────────────────────────────

func installSchedule(env scheduleEnv) (scheduleResult, error) {
	_, timing, err := normalizeInterval(env.Interval)
	if err != nil {
		return scheduleResult{}, err
	}
	if !filepath.IsAbs(env.Binary) {
		return scheduleResult{}, fmt.Errorf(
			"refusing to schedule %q: the command must be an absolute path, because a non-interactive PATH may not contain it", env.Binary)
	}
	if env.Systemctl != nil {
		return installSystemdSchedule(env, timing)
	}
	if env.CrontabRead == nil || env.CrontabWrite == nil {
		return scheduleResult{}, fmt.Errorf("no systemd user manager and no crontab available to schedule the health check")
	}
	return installCronSchedule(env, timing)
}

func scheduleCommand(env scheduleEnv) string {
	parts := append([]string{shellQuote(env.Binary)}, env.Args...)
	return strings.Join(parts, " ")
}

// shellQuote leaves ordinary paths alone — an unquoted absolute path is what
// both systemd and cron read most predictably — and quotes anything that would
// otherwise be split or expanded.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]#") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func systemdServiceUnit(env scheduleEnv) string {
	return fmt.Sprintf(`[Unit]
Description=Periscope telemetry health check
After=network-online.target

[Service]
Type=oneshot
ExecStart=%s

# No SuccessExitStatus: doctor exits 1 when a check fails, and letting systemd
# record that as a failed unit is deliberate. It is what makes the failure show
# up in "systemctl --user --failed" and in the journal instead of only in a log
# file nobody opens -- the trap that hid the original five-day outage.
`, scheduleCommand(env))
}

func systemdTimerUnit(timing scheduleTiming) string {
	return fmt.Sprintf(`[Unit]
Description=Run periscope doctor %s

[Timer]
# Not at boot exactly: the server and Claude both need a moment to exist before
# asking whether telemetry is flowing between them.
OnBootSec=5min
OnUnitActiveSec=%s
AccuracySec=1min
# Catch up on a run missed while the machine was asleep or off, so a laptop
# that is closed all weekend still reports on Monday morning.
Persistent=true
Unit=%s

[Install]
WantedBy=timers.target
`, timing.label, timing.systemd, scheduleServiceFile)
}

func installSystemdSchedule(env scheduleEnv, timing scheduleTiming) (scheduleResult, error) {
	if env.UnitDir == "" {
		return scheduleResult{}, fmt.Errorf("no systemd user unit directory")
	}
	if err := os.MkdirAll(env.UnitDir, 0755); err != nil {
		return scheduleResult{}, fmt.Errorf("mkdir %s: %w", env.UnitDir, err)
	}

	units := []struct{ name, content string }{
		{scheduleServiceFile, systemdServiceUnit(env)},
		{scheduleTimerFile, systemdTimerUnit(timing)},
	}

	changed, existed := false, false
	for _, u := range units {
		path := filepath.Join(env.UnitDir, u.name)
		old, err := os.ReadFile(path)
		if err == nil {
			existed = true
			if string(old) == u.content {
				continue // idempotence: identical content is not a rewrite
			}
		}
		if err := os.WriteFile(path, []byte(u.content), 0644); err != nil {
			return scheduleResult{}, fmt.Errorf("write %s: %w", path, err)
		}
		changed = true
	}

	// Run unconditionally, even when nothing changed: this is also the repair
	// path for a timer whose unit files are correct but which somebody
	// disabled or stopped. Both commands are idempotent.
	if out, err := env.Systemctl("daemon-reload"); err != nil {
		return scheduleResult{}, fmt.Errorf("systemctl --user daemon-reload: %s: %w", strings.TrimSpace(string(out)), err)
	}
	if out, err := env.Systemctl("enable", "--now", scheduleTimerFile); err != nil {
		return scheduleResult{}, fmt.Errorf("systemctl --user enable --now %s: %s: %w", scheduleTimerFile, strings.TrimSpace(string(out)), err)
	}

	action := "unchanged"
	switch {
	case changed && existed:
		action = "updated"
	case changed:
		action = "installed"
	}
	return scheduleResult{
		Backend: "systemd",
		Action:  action,
		Detail:  fmt.Sprintf("%s (%s) → %s", filepath.Join(env.UnitDir, scheduleTimerFile), timing.label, scheduleCommand(env)),
	}, nil
}

func cronLine(env scheduleEnv, timing scheduleTiming) string {
	// No redirect: with --quiet a healthy run prints nothing, so cron stays
	// silent, and a failing run's output becomes mail. Sending it to
	// /dev/null would rebuild the exact trap this feature exists to close.
	return fmt.Sprintf("%s %s %s", timing.cron, scheduleCommand(env), scheduleCronMarker)
}

// stripCronMarker removes every managed line and reports how many it found.
// Lines it does not own are returned byte-for-byte.
func stripCronMarker(crontab string) (string, int) {
	lines := strings.Split(crontab, "\n")
	kept := make([]string, 0, len(lines))
	removed := 0
	for _, line := range lines {
		if strings.Contains(line, scheduleCronMarker) {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	// Collapse the blank tail left behind by a removed final line.
	for strings.HasSuffix(out, "\n\n") {
		out = strings.TrimSuffix(out, "\n")
	}
	return out, removed
}

func installCronSchedule(env scheduleEnv, timing scheduleTiming) (scheduleResult, error) {
	current, err := env.CrontabRead()
	if err != nil {
		current = "" // no crontab for this user yet; that is not a failure
	}

	stripped, found := stripCronMarker(current)
	next := strings.TrimRight(stripped, "\n")
	if next != "" {
		next += "\n"
	}
	// cron drops a final line with no newline after it.
	next += cronLine(env, timing) + "\n"

	if next == current {
		return scheduleResult{Backend: "cron", Action: "unchanged", Detail: cronLine(env, timing)}, nil
	}
	if err := env.CrontabWrite(next); err != nil {
		return scheduleResult{}, fmt.Errorf("write crontab: %w", err)
	}

	action := "installed"
	if found > 0 {
		action = "updated"
	}
	return scheduleResult{
		Backend: "cron",
		Action:  action,
		Detail:  fmt.Sprintf("crontab (%s) → %s", timing.label, scheduleCommand(env)),
	}, nil
}

// ── Remove ──────────────────────────────────────────────────────────────────

// removeSchedule sweeps BOTH backends regardless of which one is available
// now. A machine that fell back to cron once and grew a systemd user manager
// later would otherwise keep a second, invisible copy of the check running.
// Removing what is not there is a no-op, not an error: uninstall is re-runnable.
func removeSchedule(env scheduleEnv) ([]string, error) {
	var removed []string
	var firstErr error

	if env.UnitDir != "" {
		present := false
		for _, name := range []string{scheduleTimerFile, scheduleServiceFile} {
			if _, err := os.Stat(filepath.Join(env.UnitDir, name)); err == nil {
				present = true
			}
		}
		// Disable BEFORE deleting: systemd cannot stop a timer whose unit file
		// has already gone, and a running timer would survive until the next
		// reboot pointing at a service that no longer exists.
		if present && env.Systemctl != nil {
			if out, err := env.Systemctl("disable", "--now", scheduleTimerFile); err != nil {
				slog.Debug("systemctl disable reported an error (the timer may never have been enabled)",
					"out", strings.TrimSpace(string(out)), "err", err)
			}
		}
		for _, name := range []string{scheduleTimerFile, scheduleServiceFile} {
			path := filepath.Join(env.UnitDir, name)
			switch err := os.Remove(path); {
			case err == nil:
				removed = append(removed, "systemd unit "+name)
			case !os.IsNotExist(err) && firstErr == nil:
				firstErr = fmt.Errorf("remove %s: %w", path, err)
			}
		}
		if present && env.Systemctl != nil {
			env.Systemctl("daemon-reload")
		}
	}

	if env.CrontabRead != nil && env.CrontabWrite != nil {
		if current, err := env.CrontabRead(); err == nil {
			if stripped, found := stripCronMarker(current); found > 0 {
				if werr := env.CrontabWrite(stripped); werr != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("write crontab: %w", werr)
					}
				} else {
					removed = append(removed, fmt.Sprintf("%s crontab entry", plural(found, "line")))
				}
			}
		}
	}

	return removed, firstErr
}

// ── Status ──────────────────────────────────────────────────────────────────

func scheduleStatus(env scheduleEnv) (bool, string, error) {
	var parts []string

	if env.UnitDir != "" {
		if _, err := os.Stat(filepath.Join(env.UnitDir, scheduleTimerFile)); err == nil {
			detail := filepath.Join(env.UnitDir, scheduleTimerFile)
			if env.Systemctl != nil {
				// A unit file that is present but not enabled runs nothing,
				// which looks exactly like health from the outside.
				if out, serr := env.Systemctl("is-enabled", scheduleTimerFile); serr == nil {
					detail += " (" + strings.TrimSpace(string(out)) + ")"
				} else {
					detail += " (NOT enabled — run `periscope schedule install`)"
				}
			}
			parts = append(parts, detail)
		}
	}

	if env.CrontabRead != nil {
		if current, err := env.CrontabRead(); err == nil {
			if _, found := stripCronMarker(current); found > 0 {
				parts = append(parts, fmt.Sprintf("%s in crontab", plural(found, "line")))
			}
		}
	}

	if len(parts) == 0 {
		return false, "no recurring health check installed", nil
	}
	return true, strings.Join(parts, "; "), nil
}

// ── Live wiring ─────────────────────────────────────────────────────────────

func liveScheduleEnv(interval string) scheduleEnv {
	return scheduleEnv{
		Binary:       absolutePeriscopeBinary(),
		Args:         scheduleDoctorArgs,
		Interval:     interval,
		UnitDir:      userSystemdUnitDir(),
		Systemctl:    systemctlUser(),
		CrontabRead:  crontabRead,
		CrontabWrite: crontabWrite,
	}
}

// absolutePeriscopeBinary is periscopeBinary() with the guarantee the scheduler
// needs. periscopeBinary falls back to a bare name when os.Executable fails,
// and a bare name in a unit file is the failure mode this whole file guards.
func absolutePeriscopeBinary() string {
	bin := periscopeBinary()
	if filepath.IsAbs(bin) {
		return bin
	}
	if abs, err := filepath.Abs(bin); err == nil {
		return abs
	}
	return bin // installSchedule rejects it, loudly
}

func userSystemdUnitDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "systemd", "user")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "systemd", "user")
}

// systemctlUser returns a runner for `systemctl --user ...`, or nil when there
// is no user manager to talk to — which is the single decision point for
// "systemd or cron?".
func systemctlUser() func(args ...string) ([]byte, error) {
	if runtime.GOOS != "linux" {
		return nil
	}
	path, err := exec.LookPath("systemctl")
	if err != nil {
		return nil
	}
	// Without a runtime dir there is no user bus, and every `--user` call
	// fails with "Failed to connect to bus" instead of doing anything.
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		return nil
	}
	out, _ := exec.Command(path, "--user", "is-system-running").CombinedOutput()
	// A user manager that is "degraded" or "starting" still accepts units, and
	// is-system-running exits non-zero for both. Only an unreachable bus or an
	// offline manager means there is nothing there.
	state := strings.ToLower(string(out))
	if strings.Contains(state, "failed to connect") || strings.Contains(state, "offline") {
		return nil
	}
	return func(args ...string) ([]byte, error) {
		return exec.Command(path, append([]string{"--user"}, args...)...).CombinedOutput()
	}
}

func crontabRead() (string, error) {
	out, err := exec.Command("crontab", "-l").CombinedOutput()
	if err != nil {
		// `crontab -l` exits non-zero when the user simply has no crontab.
		if strings.Contains(strings.ToLower(string(out)), "no crontab") {
			return "", nil
		}
		return "", fmt.Errorf("crontab -l: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

func crontabWrite(content string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("crontab -: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ── Command ─────────────────────────────────────────────────────────────────

func cmdSchedule(args []string) {
	if len(args) == 0 {
		printScheduleUsage()
		os.Exit(1)
	}

	interval := defaultScheduleInterval
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--interval":
			if i+1 >= len(rest) {
				fmt.Fprintln(os.Stderr, "--interval needs a value ("+supportedIntervals()+")")
				os.Exit(2)
			}
			interval = rest[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "unknown argument %q\n", rest[i])
			printScheduleUsage()
			os.Exit(2)
		}
	}
	if _, _, err := normalizeInterval(interval); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	env := liveScheduleEnv(interval)

	switch args[0] {
	case "install":
		res, err := installSchedule(env)
		if err != nil {
			slog.Error("schedule install failed", "err", err)
			iWarn(fmt.Sprintf("Could not install the health check: %v", err))
			os.Exit(1)
		}
		slog.Info("health check scheduled", "backend", res.Backend, "action", res.Action, "detail", res.Detail)
		iOK(fmt.Sprintf("Health check %s (%s)", res.Action, res.Backend))
		iInfo(res.Detail)
		iInfo("A failing run notifies you; see `periscope doctor --notify`.")

	case "remove", "uninstall":
		removed, err := removeSchedule(env)
		if err != nil {
			slog.Error("schedule removal failed", "err", err)
			iWarn(fmt.Sprintf("Could not fully remove the health check: %v", err))
			os.Exit(1)
		}
		if len(removed) == 0 {
			iInfo("No recurring health check was installed")
			return
		}
		slog.Info("health check removed", "removed", removed)
		iOK("Removed: " + strings.Join(removed, ", "))

	case "status":
		installed, detail, err := scheduleStatus(env)
		if err != nil {
			slog.Error("schedule status failed", "err", err)
			os.Exit(1)
		}
		if installed {
			iOK(detail)
			return
		}
		iWarn(detail)
		iInfo("Install one with `periscope schedule install`.")
		os.Exit(1)

	case "show":
		// Print exactly what would be written, and write nothing. This is the
		// reviewable form: you can read the unit before it exists.
		_, timing, _ := normalizeInterval(interval)
		fmt.Printf("# %s\n%s\n", filepath.Join(env.UnitDir, scheduleServiceFile), systemdServiceUnit(env))
		fmt.Printf("# %s\n%s\n", filepath.Join(env.UnitDir, scheduleTimerFile), systemdTimerUnit(timing))
		fmt.Printf("# crontab fallback\n%s\n", cronLine(env, timing))

	default:
		printScheduleUsage()
		os.Exit(1)
	}
}

func printScheduleUsage() {
	fmt.Println(`periscope schedule — run the health check on a timer

Usage:
  periscope schedule install [--interval 1h]  Install a systemd user timer (or a crontab line)
  periscope schedule remove                   Remove whatever was installed
  periscope schedule status                   Report what is installed
  periscope schedule show                     Print the unit/crontab content without writing it

Intervals: ` + supportedIntervals())
}

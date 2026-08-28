package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// notify.go — make a scheduled failure reach a person.
//
// A cron or timer failure that only writes to a log is the same trap that hid
// the five-day outage: the information existed, nothing carried it to a human.
// So a scheduled run escalates.
//
// Why this shape, and what was rejected:
//
//   - Adding an endpoint or a widget would mean editing server.go, push.go or
//     defaults/**, which this change does not own. Nothing in the dashboard
//     today reads an arbitrary status file, so writing one and stopping there
//     would have rebuilt the original trap with a new filename.
//
//   - The existing web-push subscription IS usable, and is used, without
//     touching push.go: sendPushNotification takes a *sql.DB, and doctor
//     already has a read-only handle on the database. Reading the VAPID keys
//     and the subscription rows needs no writes, and the notification goes to
//     the browser's push service directly, so it still arrives when the local
//     server is the thing that died. That makes push the right first choice.
//
//   - Push is silent when nobody has subscribed, so it cannot be the only
//     channel. The ladder falls through to the OS notifier, and then to stderr
//     — which under systemd is the journal AND a unit that `systemctl --user
//     --failed` lists, and under cron is mail. stderr is written on every
//     failure regardless, because it is the one channel that cannot be absent.
//
//   - The status file is still written on every run. It is not the alert; it
//     is the record, and it makes "the timer itself stopped running" provable
//     from its mtime the same way a sidecar's mtime proves a dead Stop hook.

// doctorNotifier is the escalation ladder. Every rung is a func value so the
// tests can run the whole ladder without a database, a network, or a display.
type doctorNotifier struct {
	// StatusFile is written on every run, healthy or not. Empty disables it.
	StatusFile string
	// Subscribers reports how many push subscriptions exist. Push is skipped
	// when this is zero or errors: a push nobody receives is not an alert.
	Subscribers func() (int, error)
	Push        func(title, body string) error
	Desktop     func(title, body string) error
	Stderr      io.Writer
}

// notifyDoctorRun records the run and, when it failed, escalates until one
// channel takes it. It returns the channels that actually carried something,
// which the caller logs — so "the alert went nowhere" is itself visible.
func notifyDoctorRun(n doctorNotifier, rep doctorReport) []string {
	var used []string

	if n.StatusFile != "" {
		if err := writeDoctorStatusFile(n.StatusFile, rep); err != nil {
			slog.Warn("could not write doctor status file", "path", n.StatusFile, "err", err)
		} else {
			used = append(used, "status-file")
		}
	}

	if rep.ExitCode == 0 {
		return used
	}

	title, body := doctorNotificationText(rep)

	// The floor of the ladder, written first so it survives a panic in any
	// richer channel: journal under systemd, mail under cron.
	if n.Stderr != nil {
		fmt.Fprintf(n.Stderr, "%s: %s\n", title, body)
		used = append(used, "stderr")
	}

	if n.Push != nil && n.Subscribers != nil {
		count, err := n.Subscribers()
		switch {
		case err != nil:
			slog.Warn("could not count push subscribers", "err", err)
		case count == 0:
			slog.Debug("no push subscribers; falling through to the desktop notifier")
		default:
			if perr := n.Push(title, body); perr != nil {
				slog.Warn("doctor push failed", "err", perr)
			} else {
				return append(used, "push")
			}
		}
	}

	if n.Desktop != nil {
		if err := n.Desktop(title, body); err != nil {
			slog.Warn("desktop notification failed", "err", err)
		} else {
			used = append(used, "desktop")
		}
	}
	return used
}

// doctorNotificationText names what broke and what to do about it. A push body
// that only says "unhealthy" sends the reader back to a terminal, which is the
// delay this whole feature exists to remove.
func doctorNotificationText(rep doctorReport) (title, body string) {
	var failing []string
	for _, c := range rep.Checks {
		if c.Status == "fail" {
			failing = append(failing, fmt.Sprintf("%s: %s", c.Name, c.Detail))
		}
	}
	sort.Strings(failing)

	title = fmt.Sprintf("Periscope health check FAILED (%d)", rep.Summary.Fail)
	if len(failing) == 0 {
		return title, "periscope doctor exited non-zero with no failing check recorded"
	}
	body = strings.Join(failing, "\n")
	const limit = 400 // push payloads are small; keep the first failures whole
	if len(body) > limit {
		body = body[:limit] + "…\nRun `periscope doctor` for the rest."
	}
	return title, body
}

// writeDoctorStatusFile writes the report atomically. A half-written status
// file read by anything downstream would be worse than none.
func writeDoctorStatusFile(path string, rep doctorReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ── Live wiring ─────────────────────────────────────────────────────────────

// liveDoctorNotifier builds the production ladder.
//
// Both database rungs open the file READ-ONLY, the same way every other doctor
// check does. doctor runs against an installation whose server holds the only
// writable handle; a diagnostic that takes the writer lock to report a problem
// is a diagnostic that can cause one.
func liveDoctorNotifier(env doctorEnv) doctorNotifier {
	return doctorNotifier{
		StatusFile: env.doctorStatusPath(),
		Subscribers: func() (int, error) {
			db, err := openDoctorDB(env.dbPath())
			if err != nil {
				return 0, err
			}
			defer db.Close()
			subs, err := store.PushGetAll(db)
			if err != nil {
				return 0, err
			}
			return len(subs), nil
		},
		Push: func(title, body string) error {
			db, err := openDoctorDB(env.dbPath())
			if err != nil {
				return err
			}
			defer db.Close()
			// sendPushNotification (push.go) only reads here: the VAPID keys
			// already exist — Subscribers proved the table has rows, which can
			// only happen after the server generated them — so ensureVAPIDKeys
			// takes its cached-read path and never writes.
			return sendPushNotification(db, title, body)
		},
		Desktop: desktopNotify,
		Stderr:  os.Stderr,
	}
}

// desktopNotify hands the message to whatever the OS uses for transient user
// notifications. Best effort by design: no display, no dbus, or no notifier
// binary all return an error and the ladder has already written stderr.
func desktopNotify(title, body string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf("display notification %s with title %s",
			osaQuote(body), osaQuote(title))
		cmd = exec.Command("osascript", "-e", script)
	case "windows":
		cmd = exec.Command("msg", "*", title+": "+body)
	default:
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return fmt.Errorf("notify-send not installed: %w", err)
		}
		// critical urgency so it does not auto-dismiss before it is seen.
		cmd = exec.Command(path, "--urgency=critical", "--app-name=periscope", title, body)
	}

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		return fmt.Errorf("desktop notifier timed out")
	}
}

// osaQuote renders a Go string as an AppleScript string literal.
func osaQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(s) + `"`
}

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Every test here runs against t.TempDir() or a literal result set. Nothing
// reads or writes ~/.periscope, ~/.claude, or the live server.

// fixedResults is the result set the output tests render. It deliberately holds
// one of each status so every branch of the printer is exercised by every test.
func fixedResults() []checkResult {
	return []checkResult{
		{checkHooks, ckOK, "SessionStart, Stop, UserPromptSubmit → this binary", ""},
		{checkStatusLine, ckWarn, "statusLine not set", "Run `periscope init`."},
		{checkSidecars, ckFail, "no session sidecars", "Run `periscope init`."},
	}
}

// goldenDoctorHuman is the byte-for-byte output `periscope doctor` produced
// before --json existed, captured from the pre-change printer. The whole point
// of the flag is that the humans who already read this output see no change, so
// this golden is the contract: if it moves, the flag broke something.
const goldenDoctorHuman = "\n  \x1b[90m╔═══════════════════════════════════════════╗\x1b[0m\n" +
	"  \x1b[90m║\x1b[0m  \x1b[1mP E R I S C O P E\x1b[0m                       \x1b[90m║\x1b[0m\n" +
	"  \x1b[90m║\x1b[0m  Claude Code Telemetry Dashboard          \x1b[90m║\x1b[0m\n" +
	"  \x1b[90m╚═══════════════════════════════════════════╝\x1b[0m\n\n" +
	"  \x1b[1mDiagnostics\x1b[0m  /tmp/golden/periscope\n\n" +
	"  \x1b[32m[OK]\x1b[0m  claude hooks       SessionStart, Stop, UserPromptSubmit → this binary\n" +
	"  \x1b[33m[!!]\x1b[0m  claude statusline  statusLine not set\n" +
	"                           \x1b[90m→ Run `periscope init`.\x1b[0m\n" +
	"  \x1b[31m[XX]\x1b[0m  sidecar freshness  no session sidecars\n" +
	"                           \x1b[90m→ Run `periscope init`.\x1b[0m\n\n" +
	"  \x1b[90m───────────────────────────────────────────────\x1b[0m\n\n" +
	"  \x1b[1m\x1b[31mUNHEALTHY\x1b[0m  3 checks — 1 ok, 1 warning, 1 failure\n\n"

func goldenEnv() doctorEnv {
	return doctorEnv{Now: time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC), HomeDir: "/tmp/golden/periscope"}
}

// ── Task 1: human output must not move ──────────────────────────────────────

func TestDoctorHumanOutputIsUnchangedWithoutJSON(t *testing.T) {
	var buf bytes.Buffer
	rep, code := emitDoctor(&buf, goldenEnv(), fixedResults(), doctorFlags{})
	if got := buf.String(); got != goldenDoctorHuman {
		t.Fatalf("human output changed.\n got: %q\nwant: %q", got, goldenDoctorHuman)
	}
	if code != 1 || rep.ExitCode != 1 {
		t.Fatalf("exit code = %d (report %d), want 1", code, rep.ExitCode)
	}
}

func TestDoctorHumanOutputHealthyVerdict(t *testing.T) {
	var buf bytes.Buffer
	_, code := emitDoctor(&buf, goldenEnv(), []checkResult{{checkServer, ckOK, "ok", ""}}, doctorFlags{})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), "HEALTHY") || strings.Contains(buf.String(), "UNHEALTHY") {
		t.Fatalf("healthy verdict missing from %q", buf.String())
	}
}

// ── Task 1: --json ──────────────────────────────────────────────────────────

func TestDoctorJSONIsValidAndHoldsEveryCheck(t *testing.T) {
	env := testDoctorEnv(t)
	results := runDoctorChecks(env)

	var buf bytes.Buffer
	rep, _ := emitDoctor(&buf, env, results, doctorFlags{json: true})

	var decoded doctorReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("--json did not emit valid JSON: %v\n%s", err, buf.String())
	}
	if decoded.Schema != doctorSchemaVersion {
		t.Fatalf("schema = %d, want %d", decoded.Schema, doctorSchemaVersion)
	}
	if len(decoded.Checks) != len(results) {
		t.Fatalf("JSON carries %d checks, doctor ran %d", len(decoded.Checks), len(results))
	}
	if decoded.Summary.Total != len(results) {
		t.Fatalf("summary.total = %d, want %d", decoded.Summary.Total, len(results))
	}

	// Every check doctor runs must appear, by name, with a legal status.
	seen := map[string]string{}
	for _, c := range decoded.Checks {
		seen[c.Name] = c.Status
	}
	for _, want := range []string{
		checkHooks, checkStatusLine, checkSidecars, checkIngest, checkServer,
		checkDBFile, checkDBSchema, checkDBWAL, checkLogFile, checkLogLevel,
		checkDiskHome, checkDiskData,
	} {
		status, ok := seen[want]
		if !ok {
			t.Fatalf("check %q missing from --json output", want)
		}
		switch status {
		case "ok", "warn", "fail":
		default:
			t.Fatalf("check %q has status %q, want one of ok|warn|fail", want, status)
		}
	}

	// A finding without a remediation is the failure mode doctor exists to
	// avoid; the JSON must carry it too, not just the terminal.
	for _, c := range decoded.Checks {
		if c.Status != "ok" && strings.TrimSpace(c.Remediation) == "" {
			t.Fatalf("check %q is %s in JSON but carries no remediation", c.Name, c.Status)
		}
	}
	if rep.Time == "" {
		t.Fatal("report carries no timestamp")
	}
}

func TestDoctorJSONExitCodeStillReflectsFailure(t *testing.T) {
	var buf bytes.Buffer
	rep, code := emitDoctor(&buf, goldenEnv(), fixedResults(), doctorFlags{json: true})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if rep.ExitCode != 1 || rep.Status != "unhealthy" {
		t.Fatalf("report says exit=%d status=%q, want 1/unhealthy", rep.ExitCode, rep.Status)
	}

	var decoded doctorReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded.ExitCode != 1 || decoded.Status != "unhealthy" {
		t.Fatalf("decoded exit=%d status=%q, want 1/unhealthy", decoded.ExitCode, decoded.Status)
	}
	if decoded.Summary.Fail != 1 || decoded.Summary.Warn != 1 || decoded.Summary.OK != 1 {
		t.Fatalf("summary = %+v, want 1/1/1", decoded.Summary)
	}
}

func TestDoctorJSONHealthyRunExitsZero(t *testing.T) {
	var buf bytes.Buffer
	rep, code := emitDoctor(&buf, goldenEnv(), []checkResult{
		{checkServer, ckOK, "ok", ""},
		{checkDBWAL, ckWarn, "big", "Restart the server."},
	}, doctorFlags{json: true})
	if code != 0 || rep.Status != "healthy" {
		t.Fatalf("warnings alone made doctor unhealthy: code=%d status=%q", code, rep.Status)
	}
}

func TestDoctorJSONSuppressesTheHumanReport(t *testing.T) {
	var buf bytes.Buffer
	emitDoctor(&buf, goldenEnv(), fixedResults(), doctorFlags{json: true})
	out := buf.String()
	for _, banned := range []string{"P E R I S C O P E", "\x1b[", "[XX]", "UNHEALTHY"} {
		if strings.Contains(out, banned) {
			t.Fatalf("--json output contains terminal decoration %q:\n%s", banned, out)
		}
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("--json output must end in a newline so it is line-safe for cron")
	}
}

// ── Task 1: --quiet ─────────────────────────────────────────────────────────

func TestDoctorQuietSaysNothingWhenHealthy(t *testing.T) {
	for _, f := range []doctorFlags{{quiet: true}, {quiet: true, json: true}} {
		var buf bytes.Buffer
		_, code := emitDoctor(&buf, goldenEnv(), []checkResult{{checkServer, ckOK, "ok", ""}}, f)
		if buf.Len() != 0 {
			t.Fatalf("--quiet %+v printed %q on a healthy run", f, buf.String())
		}
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	}
}

func TestDoctorQuietStillReportsFailures(t *testing.T) {
	var buf bytes.Buffer
	_, code := emitDoctor(&buf, goldenEnv(), fixedResults(), doctorFlags{quiet: true})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	out := buf.String()
	if !strings.Contains(out, checkSidecars) || !strings.Contains(out, "Run `periscope init`.") {
		t.Fatalf("--quiet dropped the failing check or its remediation:\n%s", out)
	}
	if strings.Contains(out, checkHooks) {
		t.Fatalf("--quiet printed a passing check:\n%s", out)
	}
}

func TestDoctorQuietJSONEmitsTheWholeReportOnFailure(t *testing.T) {
	var buf bytes.Buffer
	emitDoctor(&buf, goldenEnv(), fixedResults(), doctorFlags{quiet: true, json: true})
	var decoded doctorReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("--quiet --json did not emit valid JSON on failure: %v\n%s", err, buf.String())
	}
	if len(decoded.Checks) != 3 {
		t.Fatalf("--quiet --json emitted %d checks, want all 3", len(decoded.Checks))
	}
}

// ── Task 1: flag parsing ────────────────────────────────────────────────────

// A scheduled run that mails an INFO line every hour trains the reader to
// ignore its mail, which is how the one message that mattered gets missed.
func TestQuietAndJSONRunsSuppressRoutineLogging(t *testing.T) {
	for _, args := range [][]string{{"--quiet"}, {"--json"}, {"--json", "--quiet", "--notify"}} {
		if !doctorWantsQuietLogs(args) {
			t.Fatalf("doctor %v would still log an INFO line to stderr on every run", args)
		}
	}
	for _, args := range [][]string{nil, {"--notify"}, {"--bogus"}} {
		if doctorWantsQuietLogs(args) {
			t.Fatalf("doctor %v quieted logging that a human is reading", args)
		}
	}
}

func TestParseDoctorFlags(t *testing.T) {
	cases := []struct {
		args    []string
		want    doctorFlags
		wantErr bool
	}{
		{args: nil, want: doctorFlags{}},
		{args: []string{"--json"}, want: doctorFlags{json: true}},
		{args: []string{"--quiet"}, want: doctorFlags{quiet: true}},
		{args: []string{"-q"}, want: doctorFlags{quiet: true}},
		{args: []string{"--notify"}, want: doctorFlags{notify: true}},
		{args: []string{"--json", "--quiet", "--notify"}, want: doctorFlags{json: true, quiet: true, notify: true}},
		{args: []string{"--nope"}, wantErr: true},
		{args: []string{"extra"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseDoctorFlags(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseDoctorFlags(%v) accepted an unknown argument", tc.args)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseDoctorFlags(%v): %v", tc.args, err)
		}
		if got != tc.want {
			t.Fatalf("parseDoctorFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

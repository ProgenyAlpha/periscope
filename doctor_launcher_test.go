package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func launcherEnv(t *testing.T, script, binary string) doctorEnv {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, doctorLauncherName()), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return doctorEnv{HomeDir: home, Binary: binary}
}

func realBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "periscope")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The launcher pointing at this very binary is the healthy case.
func TestCheckLauncherTargetOK(t *testing.T) {
	bin := realBinary(t)
	env := launcherEnv(t, "#!/bin/sh\nnohup \""+bin+"\" serve &\n", bin)

	got := checkLauncherTarget(env)
	if got.Status != ckOK {
		t.Errorf("status = %v, detail %q, want ok", got.Status, got.Detail)
	}
}

// The failure this check was written for: init was run from a build in a
// scratch directory, so every session starts a binary that a cleanup can
// delete. Twelve other checks stayed green through it.
func TestCheckLauncherTargetFailsOnEphemeralPath(t *testing.T) {
	scratch := filepath.Join(os.TempDir(), "periscope-doctor-test", "psc")
	if err := os.MkdirAll(filepath.Dir(scratch), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(scratch)) })
	if err := os.WriteFile(scratch, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}

	env := launcherEnv(t, "#!/bin/sh\nnohup \""+scratch+"\" serve &\n", realBinary(t))

	got := checkLauncherTarget(env)
	if got.Status != ckFail {
		t.Errorf("status = %v, detail %q, want fail for a launcher pointing into a temp directory", got.Status, got.Detail)
	}
}

// A binary that is simply gone means no session will ever start the server.
func TestCheckLauncherTargetFailsOnMissingBinary(t *testing.T) {
	env := launcherEnv(t, "#!/bin/sh\nnohup \"/no/such/periscope\" serve &\n", realBinary(t))

	got := checkLauncherTarget(env)
	if got.Status != ckFail {
		t.Errorf("status = %v, want fail for a launcher whose binary does not exist", got.Status)
	}
}

// Another install of periscope in a stable location is a preference, not a
// fault, so it warns rather than fails.
func TestCheckLauncherTargetWarnsOnOtherInstall(t *testing.T) {
	// The test's own files live under a temp directory, which is the one place
	// this check treats as disqualifying. Describe them as stable for the
	// duration so the assertion is about ownership, not about /tmp.
	saved := ephemeralPrefixes
	ephemeralPrefixes = func() []string { return nil }
	t.Cleanup(func() { ephemeralPrefixes = saved })

	other := filepath.Join(t.TempDir(), "periscope")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	env := launcherEnv(t, "#!/bin/sh\nnohup \""+other+"\" serve &\n", realBinary(t))

	got := checkLauncherTarget(env)
	if got.Status != ckWarn {
		t.Errorf("status = %v, detail %q, want warn", got.Status, got.Detail)
	}
}

// A launcher that is not executable cannot start anything.
func TestCheckLauncherTargetFailsOnNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not how Windows decides this")
	}
	dead := filepath.Join(t.TempDir(), "periscope")
	if err := os.WriteFile(dead, []byte("#!/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	env := launcherEnv(t, "#!/bin/sh\nnohup \""+dead+"\" serve &\n", realBinary(t))

	got := checkLauncherTarget(env)
	if got.Status != ckFail {
		t.Errorf("status = %v, want fail for a non-executable target", got.Status)
	}
}

// Claude runs hooks through a bare /bin/sh, so people patch PATH inline on the
// hook command. Reading that assignment as the program name made periscope
// stop recognising its own Stop hook: doctor reported it missing, and `init`
// would have appended a duplicate beside a hook that was working fine.
func TestHookFieldsSkipsEnvAssignments(t *testing.T) {
	bin := realBinary(t) // sameCommand resolves the target, so it must exist
	bare := bin + " hook stop"
	prefixed := `PATH="$HOME/.local/bin:$PATH" ` + bare

	if got := hookTarget(prefixed); got != bin {
		t.Errorf("hookTarget = %q, want %q rather than the assignment", got, bin)
	}
	if got := hookArgs(prefixed); got != "hook stop" {
		t.Errorf("hookArgs = %q, want %q", got, "hook stop")
	}
	if !sameCommand(prefixed, bare) {
		t.Error("a PATH-prefixed hook is not recognised as the same hook")
	}
	if !ownedByPeriscope(prefixed, bare, hookArgs(bare)) {
		t.Error("a PATH-prefixed hook is not recognised as ours")
	}
}

// More than one assignment, and an assignment on a hook that is not ours, both
// have to come out the way they went in.
func TestHookFieldsHandlesMultipleAndForeign(t *testing.T) {
	if got := hookTarget(`FOO=1 BAR=2 /usr/bin/other --flag`); got != "/usr/bin/other" {
		t.Errorf("hookTarget = %q, want /usr/bin/other", got)
	}
	if ownedByPeriscope(`PATH=/x /usr/bin/notify send`, "/opt/periscope hook stop", "hook stop") {
		t.Error("claimed ownership of another tool's prefixed hook")
	}
	if got := hookTarget("PATH=/x"); got != "" {
		t.Errorf("hookTarget = %q, want empty for a command that is only an assignment", got)
	}
}

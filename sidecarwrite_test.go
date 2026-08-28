package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// seedSidecar writes a minimal valid sidecar so readers always have something
// well-formed to observe.
func seedSidecar(t *testing.T, path string) {
	t.Helper()
	data, err := json.Marshal(&SidecarState{Cumulative: newCumulative(), LastTurn: &LastTurn{Type: "chat"}})
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func readSidecar(t *testing.T, path string) *SidecarState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	var s SidecarState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("sidecar is not valid JSON (%d bytes): %v", len(raw), err)
	}
	return &s
}

// --- Finding 4 (writer half): atomic sidecar writes ---

// TestWriteFileAtomicNoTornReads hammers a single path with concurrent writers
// of differing payload sizes while readers poll it. With a plain os.WriteFile
// (O_TRUNC then write) readers observe zero-byte and half-written files; with
// a temp-file + rename they must never see anything but a complete document.
func TestWriteFileAtomicNoTornReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")
	seedSidecar(t, path)

	var reads, empty, unparseable int64
	stop := make(chan struct{})
	var writers, readers sync.WaitGroup

	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < 400; i++ {
				st := &SidecarState{
					Cumulative:  newCumulative(),
					LastTurn:    &LastTurn{Type: "chat"},
					FirstPrompt: strings.Repeat("x", 200+w*700),
				}
				st.Cumulative.ChatCalls = i
				data, err := json.Marshal(st)
				if err != nil {
					t.Errorf("marshal: %v", err)
					return
				}
				if err := writeFileAtomic(path, data, 0644); err != nil {
					t.Errorf("writeFileAtomic: %v", err)
					return
				}
			}
		}(w)
	}

	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					// The sidecar must never vanish: rename is atomic.
					t.Errorf("sidecar disappeared: %v", err)
					return
				}
				atomic.AddInt64(&reads, 1)
				if len(raw) == 0 {
					atomic.AddInt64(&empty, 1)
					continue
				}
				var s SidecarState
				if json.Unmarshal(raw, &s) != nil {
					atomic.AddInt64(&unparseable, 1)
				}
			}
		}()
	}

	writers.Wait()
	close(stop)
	readers.Wait()

	if reads == 0 {
		t.Fatal("readers never observed the sidecar")
	}
	if empty != 0 || unparseable != 0 {
		t.Errorf("torn sidecar reads: reads=%d empty=%d unparseable=%d", reads, empty, unparseable)
	}
	t.Logf("clean reads=%d", reads)
}

// TestWriteFileAtomicPreservesPerm: renaming a temp file into place must not
// silently widen or narrow the mode of an existing sidecar.
func TestWriteFileAtomicPreservesPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")
	if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte(`{"lastOffset":1}`), 0644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("permissions not preserved: got %o want 0600", got)
	}

	// A brand-new file gets the requested mode.
	fresh := filepath.Join(dir, "fresh.json")
	if err := writeFileAtomic(fresh, []byte("{}"), 0640); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0640 {
		t.Errorf("new file mode: got %o want 0640", got)
	}
}

// TestWriteFileAtomicLeavesNoTempFiles guards against temp-name collisions
// between concurrent writers in the same process (the old helper derived the
// temp name from the PID alone, so two goroutines shared one temp file).
func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"lastOffset":%d,"firstPrompt":%q}`, i, strings.Repeat("y", i*500))
			if err := writeFileAtomic(path, []byte(body), 0644); err != nil {
				t.Errorf("writeFileAtomic: %v", err)
			}
		}(i)
	}
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "sess.json" {
			t.Errorf("leftover file in sidecar dir: %s", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".json") && e.Name() != "sess.json" {
			t.Errorf("temp file shaped like a sidecar would poison dashboard readers: %s", e.Name())
		}
	}
	readSidecar(t, path)
}

// --- Finding 15: cross-process lost update ---

// TestUpdateSidecarStateNoLostUpdatesInProcess exercises the lock from many
// goroutines (also gives -race something to chew on).
func TestUpdateSidecarStateNoLostUpdatesInProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")
	seedSidecar(t, path)

	const goroutines, iters = 8, 40
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				err := updateSidecarState(path, func(s *SidecarState, existed bool) error {
					if !existed {
						return fmt.Errorf("sidecar vanished mid-test")
					}
					s.Cumulative.ChatCalls++
					return nil
				})
				if err != nil {
					t.Errorf("updateSidecarState: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got := readSidecar(t, path).Cumulative.ChatCalls
	if want := goroutines * iters; got != want {
		t.Errorf("lost updates: chatCalls=%d want %d (lost %d)", got, want, want-got)
	}
}

// TestSidecarLockHelperProcess is the child half of the cross-process test. It
// is a no-op unless the parent asked for it.
func TestSidecarLockHelperProcess(t *testing.T) {
	if os.Getenv("PERISCOPE_SIDECAR_HELPER") != "1" {
		t.Skip("not the helper process")
	}
	path := os.Getenv("PERISCOPE_SIDECAR_PATH")
	iters, _ := strconv.Atoi(os.Getenv("PERISCOPE_SIDECAR_ITERS"))
	title := os.Getenv("PERISCOPE_SIDECAR_TITLE")
	for i := 0; i < iters; i++ {
		err := updateSidecarState(path, func(s *SidecarState, existed bool) error {
			if !existed {
				return fmt.Errorf("sidecar missing")
			}
			// Two different writers, matching the real defect: one accumulates
			// cost/turn data (Stop hook), one stamps a generated title (server).
			if title != "" {
				s.GeneratedTitle = title
			} else {
				s.Cumulative.ChatCalls++
				s.Cumulative.Cost += 0.5
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper update failed: %v\n", err)
			os.Exit(1)
		}
	}
}

// TestSidecarLockAcrossProcesses spawns real OS processes doing the same
// read-modify-write cycle the Stop hook CLI and the server perform, and proves
// (a) no update is lost and (b) concurrent readers never see a torn file.
func TestSidecarLockAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns subprocesses")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")
	seedSidecar(t, path)

	const procs, iters = 4, 40
	const wantTitle = "generated-title"

	var reads, empty, unparseable int64
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for r := 0; r < 3; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("sidecar disappeared: %v", err)
					return
				}
				atomic.AddInt64(&reads, 1)
				if len(raw) == 0 {
					atomic.AddInt64(&empty, 1)
					continue
				}
				var s SidecarState
				if json.Unmarshal(raw, &s) != nil {
					atomic.AddInt64(&unparseable, 1)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	errs := make(chan error, procs+1)
	launch := func(title string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSidecarLockHelperProcess$", "-test.count=1", "-test.timeout=120s")
			cmd.Env = append(os.Environ(),
				"PERISCOPE_SIDECAR_HELPER=1",
				"PERISCOPE_SIDECAR_PATH="+path,
				"PERISCOPE_SIDECAR_ITERS="+strconv.Itoa(iters),
				"PERISCOPE_SIDECAR_TITLE="+title,
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("helper failed: %v\n%s", err, out)
			}
		}()
	}
	for p := 0; p < procs; p++ {
		launch("")
	}
	launch(wantTitle) // the server-side title writer, racing the hook writers
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	close(stop)
	readers.Wait()

	if reads == 0 {
		t.Fatal("readers never observed the sidecar")
	}
	if empty != 0 || unparseable != 0 {
		t.Errorf("torn cross-process reads: reads=%d empty=%d unparseable=%d", reads, empty, unparseable)
	}

	final := readSidecar(t, path)
	if want := procs * iters; final.Cumulative.ChatCalls != want {
		t.Errorf("lost updates across processes: chatCalls=%d want %d (lost %d)",
			final.Cumulative.ChatCalls, want, want-final.Cumulative.ChatCalls)
	}
	// The title writer must not have been clobbered by the cost writers, and
	// vice versa.
	if final.GeneratedTitle != wantTitle {
		t.Errorf("generated title lost: got %q want %q", final.GeneratedTitle, wantTitle)
	}
	t.Logf("clean reads=%d chatCalls=%d cost=%.1f", reads, final.Cumulative.ChatCalls, final.Cumulative.Cost)
}

// --- lock robustness ---

// TestSidecarLockRecoversFromStaleLock: a crashed holder must not deadlock
// every future writer.
func TestSidecarLockRecoversFromStaleLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")
	seedSidecar(t, path)

	lockPath := sidecarLockPath(path)
	if err := os.WriteFile(lockPath, []byte("999999 dead holder"), 0644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * sidecarLockStaleAfter)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- updateSidecarState(path, func(s *SidecarState, existed bool) error {
			s.Cumulative.ChatCalls = 7
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("update over stale lock: %v", err)
		}
	case <-time.After(sidecarLockStaleAfter + 10*time.Second):
		t.Fatal("deadlocked behind a stale lock from a crashed holder")
	}
	if got := readSidecar(t, path).Cumulative.ChatCalls; got != 7 {
		t.Errorf("chatCalls=%d want 7", got)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Errorf("lock file left behind after successful update: %v", err)
	}
}

// TestSidecarLockReleaseDoesNotDeleteStolenLock: if our lock was declared stale
// and taken over, releasing must not delete the new owner's lock.
func TestSidecarLockReleaseDoesNotDeleteStolenLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")

	lk, err := acquireSidecarLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Simulate a takeover: someone else's token now occupies the lock file.
	if err := os.WriteFile(sidecarLockPath(path), []byte("someone-else"), 0644); err != nil {
		t.Fatal(err)
	}
	lk.release()

	raw, err := os.ReadFile(sidecarLockPath(path))
	if err != nil {
		t.Fatalf("release deleted the new owner's lock: %v", err)
	}
	if string(raw) != "someone-else" {
		t.Errorf("lock contents changed: %q", raw)
	}
}

// TestSidecarLockMutualExclusion: a second acquire must block while the first
// is held, then succeed once released.
func TestSidecarLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sess.json")

	first, err := acquireSidecarLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	acquired := make(chan struct{})
	go func() {
		second, err := acquireSidecarLock(path)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			close(acquired)
			return
		}
		close(acquired)
		second.release()
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while the lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	first.release()
	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("second acquire never completed after release")
	}
}

// TestSidecarWritersUseTheSharedDiscipline is a source-level regression guard:
// neither sidecar write site may go back to a bare os.WriteFile.
func TestSidecarWritersUseTheSharedDiscipline(t *testing.T) {
	src, err := os.ReadFile("hooks.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"os.WriteFile(statePath", "os.WriteFile(path,"} {
		if strings.Contains(string(src), bad) {
			t.Errorf("hooks.go still writes the sidecar non-atomically: %s", bad)
		}
	}
}

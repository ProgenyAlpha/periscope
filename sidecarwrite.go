package main

// Durable sidecar writes.
//
// Two *separate processes* mutate ~/.claude/hooks/cost-state/<session>.json:
//
//   - the `periscope hook stop` CLI (hookStop) accumulates cost/token data, and
//   - the `periscope serve` process stamps the Haiku-generated session title
//     (generateSessionTitle, driven by the titleBackfill loop in server.go).
//
// Both do read -> unmarshal -> mutate -> write. Two hazards follow:
//
//   1. A plain os.WriteFile truncates before it writes, so any concurrent
//      reader (the dashboard payload builder, the statusline) can legitimately
//      observe a zero-byte or half-written document. Downstream one bad row
//      poisons the whole payload. Fixed by writeFileAtomic: write a temp file
//      in the same directory, fsync, then rename over the target.
//
//   2. Without cross-process serialization the two read-modify-write cycles
//      interleave and the loser's write is silently discarded, dropping either
//      the generated title or an entire turn of cumulative cost. Fixed by
//      acquireSidecarLock.
//
// Locking approach: an O_CREATE|O_EXCL lock file next to the sidecar, with
// stale-lock recovery.
//
//   - It needs no new module (flock would mean promoting golang.org/x/sys and
//     hand-writing a Windows path; periscope ships an install.ps1, so Windows
//     has to work) and O_EXCL create is atomic on every filesystem we care
//     about, including across processes.
//   - Critical sections here are short (parse the transcript tail, marshal,
//     rename) so contention is cheap to poll through.
//   - A crashed holder cannot deadlock the system: a lock whose mtime is older
//     than sidecarLockStaleAfter is broken by the next waiter, and after
//     sidecarLockTimeout a waiter force-breaks it regardless. Release is
//     token-checked so a process that was declared stale never deletes the new
//     owner's lock.
//   - Failure to lock is never allowed to drop data: updateSidecarState logs
//     and proceeds with an atomic (still tear-free) write.
//
// The lock file is "<sidecar>.json.lock" and temp files are ".<sidecar>.json.tmpNNN";
// neither ends in ".json", so the ReadDir scans that build the dashboard
// payload (which filter on a .json suffix) skip them.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	// A lock older than this is presumed abandoned by a crashed holder.
	sidecarLockStaleAfter = 15 * time.Second
	// After this long a waiter force-breaks the lock rather than block forever.
	sidecarLockTimeout = 30 * time.Second
	// Polling bounds while waiting for a live holder.
	sidecarLockMinPoll = 500 * time.Microsecond
	sidecarLockMaxPoll = 25 * time.Millisecond
)

// errSidecarNoChange lets a mutate func abort cleanly without writing.
var errSidecarNoChange = errors.New("sidecar: no change")

// writeFileAtomic writes data to a uniquely-named temp file in path's
// directory, fsyncs it, then renames it into place. rename(2) is atomic, so a
// concurrent reader sees either the old file or the new one, never a partial
// one. When path already exists its permissions are preserved; otherwise perm
// is used.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)

	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		perm = fi.Mode().Perm()
	}

	// A unique temp name per call: the previous implementation derived it from
	// the PID alone, so two goroutines in the server process writing the same
	// sidecar shared one temp file and could rename interleaved garbage into
	// place.
	f, err := os.CreateTemp(dir, "."+base+".tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // renamed away; nothing to clean up
	return nil
}

// --- cross-process advisory lock ---

type sidecarLock struct {
	path     string
	token    []byte
	released bool
}

func sidecarLockPath(target string) string { return target + ".lock" }

func newLockToken() []byte {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fall back to a nanosecond stamp; uniqueness only needs to be good
		// enough to tell "my lock" from "somebody else's".
		ns := uint64(time.Now().UnixNano())
		for i := range buf {
			buf[i] = byte(ns >> (8 * i))
		}
	}
	return []byte(fmt.Sprintf("pid=%d token=%s\n", os.Getpid(), hex.EncodeToString(buf[:])))
}

// acquireSidecarLock takes the cross-process lock guarding target. It always
// returns within roughly sidecarLockTimeout: a lock left behind by a crashed
// holder is broken rather than waited on forever.
func acquireSidecarLock(target string) (*sidecarLock, error) {
	lockPath := sidecarLockPath(target)
	token := newLockToken()
	deadline := time.Now().Add(sidecarLockTimeout)
	poll := sidecarLockMinPoll
	forced := 0

	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			if _, werr := f.Write(token); werr != nil {
				f.Close()
				os.Remove(lockPath)
				return nil, werr
			}
			if cerr := f.Close(); cerr != nil {
				os.Remove(lockPath)
				return nil, cerr
			}
			return &sidecarLock{path: lockPath, token: token}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		// Held by someone. Break it if it looks abandoned.
		if fi, serr := os.Stat(lockPath); serr == nil && time.Since(fi.ModTime()) > sidecarLockStaleAfter {
			breakStaleLock(lockPath, fi)
			continue
		}
		if time.Now().After(deadline) {
			// Never deadlock behind a holder that is alive but wedged.
			forced++
			if forced > 3 {
				return nil, fmt.Errorf("sidecar lock %s: still contended after %s", lockPath, sidecarLockTimeout)
			}
			slog.Warn("sidecar lock: force-breaking contended lock", "path", lockPath, "after", sidecarLockTimeout)
			os.Remove(lockPath)
			continue
		}

		time.Sleep(poll)
		if poll < sidecarLockMaxPoll {
			poll *= 2
		}
	}
}

// breakStaleLock removes a lock only if it still looks exactly like the stale
// one we inspected, so a lock a live process grabbed in the meantime survives.
func breakStaleLock(lockPath string, seen os.FileInfo) {
	fi, err := os.Stat(lockPath)
	if err != nil {
		return // already gone
	}
	if !fi.ModTime().Equal(seen.ModTime()) || fi.Size() != seen.Size() {
		return // refreshed or replaced since we looked
	}
	if time.Since(fi.ModTime()) <= sidecarLockStaleAfter {
		return
	}
	slog.Warn("sidecar lock: breaking stale lock from a crashed holder",
		"path", lockPath, "age", time.Since(fi.ModTime()).Round(time.Millisecond))
	os.Remove(lockPath)
}

// release drops the lock. It is idempotent, and it refuses to delete a lock
// file that no longer holds our token — i.e. one that was broken as stale and
// re-taken by another writer.
func (l *sidecarLock) release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	if cur, err := os.ReadFile(l.path); err == nil && !bytes.Equal(cur, l.token) {
		return
	}
	os.Remove(l.path)
}

// --- locked read-modify-write ---

// writeSidecarState marshals and atomically writes state. Call it while
// holding the sidecar lock.
func writeSidecarState(path string, state *SidecarState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0644)
}

// updateSidecarState performs the whole read -> mutate -> write cycle under the
// cross-process lock, so the Stop hook CLI and the server never clobber each
// other. mutate receives the state loaded from disk and whether the sidecar
// already existed and parsed; returning errSidecarNoChange skips the write.
//
// If the lock cannot be taken the update still goes through (atomically, so no
// reader ever tears) rather than dropping a turn of data on the floor.
func updateSidecarState(path string, mutate func(state *SidecarState, existed bool) error) error {
	lk, err := acquireSidecarLock(path)
	if err != nil {
		slog.Warn("sidecar lock unavailable, writing unlocked", "path", filepath.Base(path), "err", err)
	} else {
		defer lk.release()
	}

	state, existed := loadSidecarState(path)
	if err := mutate(state, existed); err != nil {
		if errors.Is(err, errSidecarNoChange) {
			return nil
		}
		return err
	}
	return writeSidecarState(path, state)
}

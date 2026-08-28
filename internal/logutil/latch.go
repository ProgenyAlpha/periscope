// Package logutil holds small logging helpers shared across periscope.
package logutil

import "sync"

// Latch suppresses log spam from a condition that repeats every poll cycle.
//
// Periscope's poll loop, watcher and HTTP handlers all re-run the same import
// work at full cadence, so a persistent failure (a missing data directory, an
// unwritable cache file) used to emit one identical ERROR per cycle — 6,671
// copies of a single condition in one log file. Latch turns a condition into
// edges: one line when it starts, one line when its cause changes and one line
// when it clears, with a count of everything suppressed in between.
//
// The zero value is ready to use and safe for concurrent use.
type Latch struct {
	mu    sync.Mutex
	state map[string]*condition
}

type condition struct {
	cause      string
	suppressed int
}

// Fail records that key is currently failing with the given cause.
//
// It reports first=true on the transition into failure and whenever the cause
// changes — those are the only calls that should emit a log line. While the
// identical cause repeats it reports first=false and n, the number of
// suppressed repeats since the condition was last logged.
func (l *Latch) Fail(key, cause string) (first bool, n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		l.state = make(map[string]*condition)
	}
	c, ok := l.state[key]
	if !ok || c.cause != cause {
		l.state[key] = &condition{cause: cause}
		return true, 0
	}
	c.suppressed++
	return false, c.suppressed
}

// OK records that key is currently healthy.
//
// It reports recovered=true exactly once per failure episode, along with the
// number of repeats that were suppressed while the condition persisted, so the
// caller can log a single "recovered" line. Calls on an already-healthy key
// report false and cost nothing.
func (l *Latch) OK(key string) (recovered bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.state[key]
	if !ok {
		return false, 0
	}
	delete(l.state, key)
	return true, c.suppressed
}

package logutil

import (
	"sync"
	"testing"
)

// Finding 6: repeated identical failures must log once on transition, not
// once per poll cycle.
func TestLatch_FailLogsOnceUntilCauseChanges(t *testing.T) {
	var l Latch

	if first, n := l.Fail("sidecars", "no such file"); !first || n != 0 {
		t.Fatalf("first failure: got (first=%v, n=%d), want (true, 0)", first, n)
	}
	for i := 1; i <= 5; i++ {
		first, n := l.Fail("sidecars", "no such file")
		if first {
			t.Fatalf("repeat %d: expected suppression, got a new log line", i)
		}
		if n != i {
			t.Fatalf("repeat %d: suppressed count = %d, want %d", i, n, i)
		}
	}

	// A different cause is a new condition and must log again.
	if first, _ := l.Fail("sidecars", "permission denied"); !first {
		t.Fatal("changed cause: expected a new log line")
	}
	if first, _ := l.Fail("sidecars", "permission denied"); first {
		t.Fatal("repeat of changed cause: expected suppression")
	}
}

func TestLatch_KeysAreIndependent(t *testing.T) {
	var l Latch
	l.Fail("a", "boom")
	if first, _ := l.Fail("b", "boom"); !first {
		t.Fatal("distinct key must log independently")
	}
}

func TestLatch_OKReportsRecoveryOnce(t *testing.T) {
	var l Latch

	if ok, n := l.OK("sidecars"); ok || n != 0 {
		t.Fatalf("OK with no prior failure: got (%v,%d), want (false,0)", ok, n)
	}

	l.Fail("sidecars", "boom")
	l.Fail("sidecars", "boom")
	l.Fail("sidecars", "boom")

	ok, n := l.OK("sidecars")
	if !ok {
		t.Fatal("expected recovery to be reported")
	}
	// Three failures: one logged, two suppressed.
	if n != 2 {
		t.Fatalf("suppressed count on recovery = %d, want 2", n)
	}

	if ok, _ := l.OK("sidecars"); ok {
		t.Fatal("second OK must not report recovery again")
	}

	// After recovery the next failure is a fresh transition.
	if first, n := l.Fail("sidecars", "boom"); !first || n != 0 {
		t.Fatalf("post-recovery failure: got (%v,%d), want (true,0)", first, n)
	}
}

func TestLatch_ConcurrentUse(t *testing.T) {
	var l Latch
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				l.Fail("k", "cause")
				l.OK("other")
			}
		}()
	}
	wg.Wait()
}

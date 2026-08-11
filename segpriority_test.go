package main

import "testing"

// With uniform priorities the truncation loop always removed index 0, so the
// leftmost segment was sacrificed first no matter what it was.
func TestDefaultPrioritiesKeepIdentityOverDetail(t *testing.T) {
	keep := []string{"dir", "model", "cost", "rate-5hr", "rate-weekly"}
	drop := []string{"tools", "turns", "proj", "burn", "cache"}
	for _, k := range keep {
		for _, d := range drop {
			if defaultPriority(k) >= defaultPriority(d) {
				t.Errorf("%s (p%d) should outrank %s (p%d)", k, defaultPriority(k), d, defaultPriority(d))
			}
		}
	}
}

func TestEverySegmentHasAPriority(t *testing.T) {
	for _, n := range []string{"dir", "git", "model", "effort", "fast", "turns", "cost", "burn",
		"tools", "vim", "rate-5hr", "rate-weekly", "rate-scoped", "reset", "proj", "cache", "context"} {
		if p := defaultPriority(n); p < 1 || p > 9 {
			t.Errorf("segment %q has priority %d, want 1..9", n, p)
		}
	}
}

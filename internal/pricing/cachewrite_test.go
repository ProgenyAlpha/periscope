package pricing

import (
	"path/filepath"
	"testing"
)

// The 24h pricing cache is written into ~/.claude/hooks/cost-state, which
// `periscope init` did not create. os.WriteFile does not create parent
// directories and its error was discarded as "non-fatal", so the cache never
// appeared: every /api/pricing hit and every statusline overlay load re-fetched
// from GitHub, and LoadOverlay always returned nil.
func TestWritePricingCache_CreatesMissingDataDir(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "hooks", "cost-state")

	models := map[string]any{
		"claude-opus-4": map[string]any{"input": 15.0, "output": 75.0, "cache_read": 1.5, "cache_write": 18.75},
	}
	if err := writePricingCache(dataDir, models); err != nil {
		t.Fatalf("writePricingCache into a missing directory: %v", err)
	}

	rates := LoadOverlay(dataDir)
	if rates == nil {
		t.Fatal("LoadOverlay found no cache after the write")
	}
	got, ok := rates["claude-opus-4"]
	if !ok {
		t.Fatalf("model missing from the overlay: %+v", rates)
	}
	if got.Input != 15.0 || got.Output != 75.0 || got.CacheRead != 1.5 {
		t.Errorf("rates round-tripped wrong: %+v", got)
	}
}

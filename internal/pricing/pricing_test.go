package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetRates_ExactMatch(t *testing.T) {
	tests := []struct {
		model string
		want  ModelRates
	}{
		{"claude-opus-4-6", ModelRates{5, 0.50, 6.25, 10, 25}},
		{"claude-haiku-4-5-20251001", ModelRates{1, 0.10, 1.25, 2, 5}},
		{"claude-sonnet-4-5-20250929", ModelRates{3, 0.30, 3.75, 6, 15}},
		{"claude-3-haiku", ModelRates{0.25, 0.03, 0.30, 0.50, 1.25}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetRates(tt.model)
			if got != tt.want {
				t.Errorf("GetRates(%q) = %+v, want %+v", tt.model, got, tt.want)
			}
		})
	}
}

func TestGetRates_PrefixMatch(t *testing.T) {
	tests := []struct {
		model      string
		wantInput  float64
		wantPrefix string
	}{
		{"claude-3-5-sonnet-20241022", 3, "claude-3-5-sonnet"},
		{"claude-3-haiku-20240307", 0.25, "claude-3-haiku"},
		{"claude-3-5-haiku-20241022", 0.80, "claude-3-5-haiku"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := GetRates(tt.model)
			if got.Input != tt.wantInput {
				t.Errorf("GetRates(%q).Input = %v, want %v (prefix %s)", tt.model, got.Input, tt.wantInput, tt.wantPrefix)
			}
		})
	}
}

func TestGetRates_UnknownFallback(t *testing.T) {
	fallback := ModelPricing["claude-opus-4-6"]
	tests := []string{"gpt-4", "gemini-pro", "llama-3", ""}
	for _, model := range tests {
		t.Run(model, func(t *testing.T) {
			got := GetRates(model)
			if got != fallback {
				t.Errorf("GetRates(%q) = %+v, want fallback %+v", model, got, fallback)
			}
		})
	}
}

func TestGetRates_ClaudeSonnet5(t *testing.T) {
	got := GetRates("claude-sonnet-5")
	want := ModelRates{Input: 3, CacheRead: 0.30, CacheWrite: 3.75, CacheWrite1h: 6, Output: 15}
	if got != want {
		t.Errorf("GetRates(claude-sonnet-5) = %+v, want %+v", got, want)
	}
}

func TestGetRates_ClaudeHaiku45Bare(t *testing.T) {
	got := GetRates("claude-haiku-4-5")
	want := ModelRates{Input: 1, CacheRead: 0.10, CacheWrite: 1.25, CacheWrite1h: 2, Output: 5}
	if got != want {
		t.Errorf("GetRates(claude-haiku-4-5) = %+v, want %+v", got, want)
	}
}

func TestGetRates_ClaudeOpus5(t *testing.T) {
	got := GetRates("claude-opus-5")
	want := ModelRates{Input: 5, CacheRead: 0.50, CacheWrite: 6.25, CacheWrite1h: 10, Output: 25}
	if got != want {
		t.Errorf("GetRates(claude-opus-5) = %+v, want %+v", got, want)
	}
}

func TestModelPricing_AllNonZeroInput(t *testing.T) {
	for model, rates := range ModelPricing {
		if rates.Input <= 0 {
			t.Errorf("ModelPricing[%q].Input = %v, want > 0", model, rates.Input)
		}
	}
}

func TestModelPricing_CacheWrite1hIsDoubleInput(t *testing.T) {
	for model, rates := range ModelPricing {
		want := rates.Input * 2.0
		if rates.CacheWrite1h != want {
			t.Errorf("ModelPricing[%q].CacheWrite1h = %v, want %v (2x input)", model, rates.CacheWrite1h, want)
		}
	}
}

func TestTokenWeights(t *testing.T) {
	if TokenWeights.Input != 1.0 {
		t.Errorf("TokenWeights.Input = %v, want 1.0", TokenWeights.Input)
	}
	if TokenWeights.CacheRead != 0 {
		t.Errorf("TokenWeights.CacheRead = %v, want 0", TokenWeights.CacheRead)
	}
	if TokenWeights.Output != 5.0 {
		t.Errorf("TokenWeights.Output = %v, want 5.0", TokenWeights.Output)
	}
}

func TestLoadOverlay_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if overlay := LoadOverlay(dir); overlay != nil {
		t.Errorf("LoadOverlay(missing) = %v, want nil", overlay)
	}
}

func TestLoadOverlay_CorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "litellm-pricing-cache.json"), []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if overlay := LoadOverlay(dir); overlay != nil {
		t.Errorf("LoadOverlay(corrupt) = %v, want nil", overlay)
	}
}

func TestLoadOverlay_SkipsNonPositiveInput(t *testing.T) {
	dir := t.TempDir()
	cache := `{"fetched_at": 9999999999, "data": {
		"claude-opus-5": {"input": 5.5, "output": 27.5, "cache_read": 0.55, "cache_write": 6.875},
		"claude-sonnet-5": {"input": 0, "output": 15, "cache_read": 0.3, "cache_write": 3.75}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "litellm-pricing-cache.json"), []byte(cache), 0644); err != nil {
		t.Fatal(err)
	}
	overlay := LoadOverlay(dir)
	want := ModelRates{Input: 5.5, CacheRead: 0.55, CacheWrite: 6.875, CacheWrite1h: 11, Output: 27.5}
	if got := overlay["claude-opus-5"]; got != want {
		t.Errorf("overlay[claude-opus-5] = %+v, want %+v", got, want)
	}
	if _, ok := overlay["claude-sonnet-5"]; ok {
		t.Errorf("overlay[claude-sonnet-5] present with input<=0, want skipped")
	}
}

func TestMergeOverlay_MissingFileLeavesHardcodedIntact(t *testing.T) {
	dir := t.TempDir() // no cache file present
	merged := mergeOverlay(ModelPricing, dir)
	for model, want := range ModelPricing {
		if got := merged[model]; got != want {
			t.Errorf("merged[%q] = %+v, want hardcoded %+v", model, got, want)
		}
	}
}

func TestMergeOverlay_CorruptFileLeavesHardcodedIntact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "litellm-pricing-cache.json"), []byte("{not valid"), 0644); err != nil {
		t.Fatal(err)
	}
	merged := mergeOverlay(ModelPricing, dir)
	for model, want := range ModelPricing {
		if got := merged[model]; got != want {
			t.Errorf("merged[%q] = %+v, want hardcoded %+v", model, got, want)
		}
	}
}

func TestMergeOverlay_NonPositiveInputKeepsHardcoded(t *testing.T) {
	dir := t.TempDir()
	cache := `{"fetched_at": 9999999999, "data": {
		"claude-opus-5": {"input": 0, "output": 25, "cache_read": 0.5, "cache_write": 6.25}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "litellm-pricing-cache.json"), []byte(cache), 0644); err != nil {
		t.Fatal(err)
	}
	merged := mergeOverlay(ModelPricing, dir)
	if got := merged["claude-opus-5"]; got != ModelPricing["claude-opus-5"] {
		t.Errorf("merged[claude-opus-5] = %+v, want hardcoded %+v (bad feed entry)", got, ModelPricing["claude-opus-5"])
	}
}

func TestMergeOverlay_ValidEntryOverridesHardcoded(t *testing.T) {
	dir := t.TempDir()
	cache := `{"fetched_at": 9999999999, "data": {
		"claude-opus-5": {"input": 4, "output": 20, "cache_read": 0.4, "cache_write": 5}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "litellm-pricing-cache.json"), []byte(cache), 0644); err != nil {
		t.Fatal(err)
	}
	merged := mergeOverlay(ModelPricing, dir)
	want := ModelRates{Input: 4, CacheRead: 0.4, CacheWrite: 5, CacheWrite1h: 8, Output: 20}
	if got := merged["claude-opus-5"]; got != want {
		t.Errorf("merged[claude-opus-5] = %+v, want overlaid %+v", got, want)
	}
	// untouched models keep their hardcoded rates
	if got := merged["claude-sonnet-5"]; got != ModelPricing["claude-sonnet-5"] {
		t.Errorf("merged[claude-sonnet-5] = %+v, want hardcoded %+v", got, ModelPricing["claude-sonnet-5"])
	}
}

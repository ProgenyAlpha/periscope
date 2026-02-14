package pricing

import "testing"

func TestGetRates_ExactMatch(t *testing.T) {
	tests := []struct {
		model string
		want  ModelRates
	}{
		{"claude-opus-4-6", ModelRates{5, 0.50, 6.25, 25}},
		{"claude-haiku-4-5-20251001", ModelRates{1, 0.10, 1.25, 5}},
		{"claude-sonnet-4-5-20250929", ModelRates{3, 0.30, 3.75, 15}},
		{"claude-3-haiku", ModelRates{0.25, 0.03, 0.30, 1.25}},
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

func TestModelPricing_AllNonZeroInput(t *testing.T) {
	for model, rates := range ModelPricing {
		if rates.Input <= 0 {
			t.Errorf("ModelPricing[%q].Input = %v, want > 0", model, rates.Input)
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

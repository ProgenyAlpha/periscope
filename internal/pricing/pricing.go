package pricing

import "strings"

// ModelRates defines the cost per million tokens (or unit) for different operations.
type ModelRates struct {
	Input      float64
	CacheRead  float64
	CacheWrite float64
	Output     float64
}

// ModelPricing defines the rates for known Claude models.
// Rates are typically in $ per million tokens.
var ModelPricing = map[string]ModelRates{
	"claude-3-opus":              {15, 0.75, 3.75, 75}, // Example generic Opus
	"claude-3-sonnet":            {3, 0.30, 3.75, 15},  // Example generic Sonnet
	"claude-3-haiku":             {0.25, 0.03, 0.30, 1.25},
	"claude-3-5-sonnet":          {3, 0.30, 3.75, 15},
	"claude-3-5-haiku":           {0.80, 0.08, 1.00, 4},
    // Legacy/specific versions from hooks.go
	"claude-opus-4-6":            {5, 0.50, 6.25, 25},
	"claude-opus-4-5":            {5, 0.50, 6.25, 25},
	"claude-opus-4-1":            {15, 1.50, 18.75, 75},
	"claude-sonnet-4-5-20250929": {3, 0.30, 3.75, 15},
	"claude-haiku-4-5-20251001":  {1, 0.10, 1.25, 5},
	"claude-haiku-3-5":           {0.80, 0.08, 1.00, 4},
}

// TokenWeights defines the rate-limit token weights.
// cache_read=0 (doesn't count toward ITPM), output=5 (OTPM limits ~5x tighter).
var TokenWeights = struct {
	Input, CacheRead, CacheWrite, Output float64
}{1.0, 0, 1.0, 5.0}

// GetRates return the pricing rates for a given model ID.
// It matches by prefix if an exact match isn't found.
func GetRates(model string) ModelRates {
	if rates, ok := ModelPricing[model]; ok {
		return rates
	}
	for prefix, rates := range ModelPricing {
		if strings.HasPrefix(model, prefix) {
			return rates
		}
	}
    // Fallback default (Opus pricing often safest heavy estimate or Sonnet as middle ground)
	return ModelPricing["claude-opus-4-6"] 
}

package pricing

import (
	"strings"
	"sync"
)

// ModelRates defines the cost per million tokens (or unit) for different operations.
type ModelRates struct {
	Input        float64
	CacheRead    float64
	CacheWrite   float64 // 5-minute TTL cache write rate
	CacheWrite1h float64 // 1-hour TTL cache write rate
	Output       float64
}

// ModelPricing defines the rates for known Claude models.
// Rates are typically in $ per million tokens.
var ModelPricing = map[string]ModelRates{
	"claude-3-opus":     {15, 1.50, 18.75, 30, 75}, // Example generic Opus
	"claude-3-sonnet":   {3, 0.30, 3.75, 6, 15},    // Example generic Sonnet
	"claude-3-haiku":    {0.25, 0.03, 0.30, 0.50, 1.25},
	"claude-3-5-sonnet": {3, 0.30, 3.75, 6, 15},
	"claude-3-5-haiku":  {0.80, 0.08, 1.00, 1.60, 4},
	// Legacy/specific versions from hooks.go
	"claude-fable-5":             {10, 1.00, 12.50, 20, 50},
	"claude-mythos-5":            {10, 1.00, 12.50, 20, 50},
	"claude-opus-5":              {5, 0.50, 6.25, 10, 25},
	"claude-sonnet-5":            {3, 0.30, 3.75, 6, 15},
	"claude-haiku-4-5":           {1, 0.10, 1.25, 2, 5},
	"claude-sonnet-4-6":          {3, 0.30, 3.75, 6, 15},
	"claude-opus-4-8":            {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-7":            {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-6":            {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-5":            {5, 0.50, 6.25, 10, 25},
	"claude-opus-4-1":            {15, 1.50, 18.75, 30, 75},
	"claude-sonnet-4-5-20250929": {3, 0.30, 3.75, 6, 15},
	"claude-haiku-4-5-20251001":  {1, 0.10, 1.25, 2, 5},
	"claude-haiku-3-5":           {0.80, 0.08, 1.00, 1.60, 4},
}

// TokenWeights defines the rate-limit token weights.
// cache_read=0 (doesn't count toward ITPM), output=5 (OTPM limits ~5x tighter).
var TokenWeights = struct {
	Input, CacheRead, CacheWrite, Output float64
}{1.0, 0, 1.0, 5.0}

var (
	overlayOnce    sync.Once
	overlayDataDir string
	effectiveRates map[string]ModelRates
)

// InitOverlay sets the data directory the live LiteLLM pricing overlay is
// loaded from. Must be called (if at all) before the first GetRates call —
// the overlay is loaded lazily, once, on first use.
func InitOverlay(dataDir string) {
	overlayDataDir = dataDir
}

// mergeOverlay returns a copy of base with any valid entries from the
// LiteLLM pricing cache in dataDir layered on top. Missing, unreadable, or
// malformed cache data — or an entry with a non-positive input rate — leaves
// the corresponding base entry untouched.
func mergeOverlay(base map[string]ModelRates, dataDir string) map[string]ModelRates {
	merged := make(map[string]ModelRates, len(base))
	for model, rates := range base {
		merged[model] = rates
	}
	if dataDir == "" {
		return merged
	}
	for model, rates := range LoadOverlay(dataDir) {
		merged[model] = rates
	}
	return merged
}

func ratesTable() map[string]ModelRates {
	overlayOnce.Do(func() {
		effectiveRates = mergeOverlay(ModelPricing, overlayDataDir)
	})
	return effectiveRates
}

// GetRates return the pricing rates for a given model ID.
// It matches by prefix if an exact match isn't found.
func GetRates(model string) ModelRates {
	pricing := ratesTable()

	if rates, ok := pricing[model]; ok {
		return rates
	}
	bestLen := -1
	var bestRates ModelRates
	for prefix, rates := range pricing {
		if strings.HasPrefix(model, prefix) && len(prefix) > bestLen {
			bestLen = len(prefix)
			bestRates = rates
		}
	}
	if bestLen >= 0 {
		return bestRates
	}
	// Fallback default (Opus pricing often safest heavy estimate or Sonnet as middle ground)
	return pricing["claude-opus-4-6"]
}

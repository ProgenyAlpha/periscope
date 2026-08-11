package pricing

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FetchLiteLLMPricing fetches Claude model pricing from LiteLLM's GitHub source, with 24h cache.
func FetchLiteLLMPricing(dataDir string) (json.RawMessage, error) {
	cachePath := filepath.Join(dataDir, "litellm-pricing-cache.json")
	if data, err := os.ReadFile(cachePath); err == nil {
		var cache struct {
			FetchedAt int64           `json:"fetched_at"`
			Data      json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &cache) == nil {
			if time.Since(time.Unix(cache.FetchedAt, 0)) < 24*time.Hour {
				return cache.Data, nil
			}
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json")
	if err != nil {
		return readCacheFallback(cachePath)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return readCacheFallback(cachePath)
	}

	var allModels map[string]map[string]any
	if err := json.Unmarshal(body, &allModels); err != nil {
		return readCacheFallback(cachePath)
	}

	result := make(map[string]any)
	for name, info := range allModels {
		if !strings.HasPrefix(name, "claude-") {
			continue
		}
		if strings.Contains(name, "bedrock") || strings.Contains(name, "vertex") {
			continue
		}
		model := map[string]any{}
		if v, ok := info["input_cost_per_token"].(float64); ok {
			model["input"] = v * 1e6
		}
		if v, ok := info["output_cost_per_token"].(float64); ok {
			model["output"] = v * 1e6
		}
		if v, ok := info["cache_read_input_token_cost"].(float64); ok {
			model["cache_read"] = v * 1e6
		}
		if v, ok := info["cache_creation_input_token_cost"].(float64); ok {
			model["cache_write"] = v * 1e6
		}
		if v, ok := info["max_input_tokens"].(float64); ok {
			model["max_input"] = int(v)
		}
		if v, ok := info["max_output_tokens"].(float64); ok {
			model["max_output"] = int(v)
		}
		result[name] = model
	}

	data, _ := json.Marshal(result)
	cache := map[string]any{"fetched_at": time.Now().Unix(), "data": result}
	cacheData, _ := json.Marshal(cache)
	os.WriteFile(cachePath, cacheData, 0644) // non-fatal: data still returned

	return data, nil
}

// LoadOverlay reads the LiteLLM pricing cache file from dataDir and returns
// the model rates found there. It never makes a network call. A missing,
// unreadable, or malformed file returns nil. An entry with a non-positive
// input rate is skipped. The feed doesn't supply a 1h cache-write rate, so
// it's derived as 2x the feed's input rate.
func LoadOverlay(dataDir string) map[string]ModelRates {
	cachePath := filepath.Join(dataDir, "litellm-pricing-cache.json")
	raw, err := os.ReadFile(cachePath)
	if err != nil {
		return nil
	}

	var cache struct {
		Data map[string]struct {
			Input      float64 `json:"input"`
			Output     float64 `json:"output"`
			CacheRead  float64 `json:"cache_read"`
			CacheWrite float64 `json:"cache_write"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &cache) != nil {
		return nil
	}

	rates := make(map[string]ModelRates, len(cache.Data))
	for model, entry := range cache.Data {
		if entry.Input <= 0 {
			continue
		}
		rates[model] = ModelRates{
			Input:        entry.Input,
			CacheRead:    entry.CacheRead,
			CacheWrite:   entry.CacheWrite,
			CacheWrite1h: entry.Input * 2.0,
			Output:       entry.Output,
		}
	}
	return rates
}

func readCacheFallback(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return json.RawMessage("{}"), nil
	}
	var cache struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(data, &cache) == nil && cache.Data != nil {
		return cache.Data, nil
	}
	return json.RawMessage("{}"), nil
}

package pricing

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	// Non-fatal: the caller still gets the freshly fetched data. But it was
	// silently non-functional too — dataDir may not exist on a fresh install,
	// so the cache never landed and every call re-fetched from GitHub.
	if err := writePricingCache(dataDir, result); err != nil {
		slog.Warn("pricing cache write failed", "dir", dataDir, "err", err)
	}

	return data, nil
}

// writePricingCache stores the 24h model-rate cache. It creates the data
// directory if it is missing and swaps the file in with a rename, because the
// statusline reads it (LoadOverlay) from a different process.
func writePricingCache(dataDir string, models map[string]any) error {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	cacheData, err := json.Marshal(map[string]any{
		"fetched_at": time.Now().Unix(),
		"data":       models,
	})
	if err != nil {
		return err
	}

	path := filepath.Join(dataDir, "litellm-pricing-cache.json")
	f, err := os.CreateTemp(dataDir, ".litellm-pricing-cache.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(cacheData); err != nil {
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
	if err := os.Chmod(tmp, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = ""
	return nil
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

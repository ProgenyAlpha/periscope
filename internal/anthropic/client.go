package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// APIError represents a non-200 response from the Anthropic API.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("API returned %d: %s", e.StatusCode, e.Body)
}

// IsAuthError returns true if the error is a 401 Unauthorized.
func IsAuthError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 401
}

// IsRateLimited returns true if the error is a 429 Too Many Requests.
func IsRateLimited(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == 429
}

// Client handles communication with the Anthropic API.
type Client struct {
	Token string
}

// UsageWindow represents dynamic usage data from Anthropic.
type UsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

// LimitEntry is one row of the `limits` array returned by /api/oauth/usage.
// Anthropic added it alongside the flat seven_day_* fields and, as of
// 2026-06-30, uses it as the only carrier for per-model weekly caps:
// seven_day_sonnet and seven_day_opus now return null, while a
// kind="weekly_scoped" entry reports the capped model in scope.model.
type LimitEntry struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt string  `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    *struct {
		Model *struct {
			ID          *string `json:"id"`
			DisplayName string  `json:"display_name"`
		} `json:"model"`
		Surface *string `json:"surface"`
	} `json:"scope"`
}

// APIResponse represents the usage API response structure.
type APIResponse struct {
	FiveHour          *UsageWindow `json:"five_hour"`
	SevenDay          *UsageWindow `json:"seven_day"`
	SevenDaySonnet    *UsageWindow `json:"seven_day_sonnet"`
	SevenDayOpus      *UsageWindow `json:"seven_day_opus"`
	SevenDayOauthApps *UsageWindow `json:"seven_day_oauth_apps"`
	SevenDayCowork    *UsageWindow `json:"seven_day_cowork"`
	Limits            []LimitEntry `json:"limits"`
	ExtraUsage        *struct {
		IsEnabled    bool     `json:"is_enabled"`
		MonthlyLimit *float64 `json:"monthly_limit"`
		UsedCredits  float64  `json:"used_credits"`
		Utilization  *float64 `json:"utilization"`
	} `json:"extra_usage"`
	// Spend is the credits object /api/oauth/usage started returning
	// alongside extra_usage. Amounts are in minor units (e.g. cents); the
	// exponent says how many places to shift, and it isn't always 2.
	Spend *struct {
		Used *struct {
			AmountMinor float64 `json:"amount_minor"`
			Exponent    int     `json:"exponent"`
		} `json:"used"`
		Limit *struct {
			AmountMinor float64 `json:"amount_minor"`
			Exponent    int     `json:"exponent"`
		} `json:"limit"`
		Percent        float64 `json:"percent"`
		Enabled        bool    `json:"enabled"`
		DisabledReason *string `json:"disabled_reason"`
	} `json:"spend"`
	// OverageUtilization is populated only by parseUnifiedHeaders. Used to
	// derive UsedCredits from a cached monthly_limit cap when the dedicated
	// /api/oauth/usage endpoint isn't being called.
	OverageUtilization *float64 `json:"-"`
	// RepresentativeClaim, OverageStatus, and OverageDisabledReason are
	// populated only by parseUnifiedHeaders.
	RepresentativeClaim   *string `json:"-"`
	OverageStatus         *string `json:"-"`
	OverageDisabledReason *string `json:"-"`
}

// NewClientFromDisk reads credentials from the standard Claude location.
func NewClientFromDisk(claudeDir string) (*Client, error) {
	credPath := filepath.Join(claudeDir, ".credentials.json")
	data, err := os.ReadFile(credPath)
	if err != nil {
		return nil, fmt.Errorf("credentials not found: %w", err)
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("credentials parse error: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return nil, fmt.Errorf("no OAuth token found")
	}
	return &Client{Token: creds.ClaudeAiOauth.AccessToken}, nil
}

// FetchUsage retrieves current usage stats.
func (c *Client) FetchUsage() (*APIResponse, error) {
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var parsed APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return &parsed, nil
}

// FetchUsageFromHeaders sends a minimal /v1/messages ping and reads the
// unified rate-limit headers Anthropic returns on every message response.
//
// This is the preferred path: /api/oauth/usage has a tight ~5-req-per-90min
// budget and 429s force a long cooldown, while /v1/messages doesn't gate on
// metering frequency — only on the same 5h/7d windows we're trying to read.
// One ping costs ≈ $0.00001 (1 input + 1 output token on the cheapest model).
//
// Returns an APIResponse populated from headers so callers don't need to
// know which fetch path produced the data.
func (c *Client) FetchUsageFromHeaders() (*APIResponse, error) {
	body := bytes.NewReader([]byte(`{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"."}]}`))
	req, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", body)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain so the connection can be reused

	// Even on 429 the rate-limit headers are returned and are still authoritative.
	parsed := parseUnifiedHeaders(resp.Header)
	if parsed == nil {
		if resp.StatusCode != 200 {
			return nil, &APIError{StatusCode: resp.StatusCode, Body: "no unified rate-limit headers in response"}
		}
		return nil, fmt.Errorf("no unified rate-limit headers in response")
	}
	return parsed, nil
}

// parseUnifiedHeaders maps anthropic-ratelimit-unified-* headers into the
// APIResponse shape used by TransformUsage. Header names per Claude Code
// issue #12829. Note 7d_sonnet uses an underscore, not a dash.
func parseUnifiedHeaders(h http.Header) *APIResponse {
	get := func(k string) string { return h.Get("anthropic-ratelimit-unified-" + k) }
	parsePair := func(utilKey, resetKey string) *UsageWindow {
		u := get(utilKey)
		r := get(resetKey)
		if u == "" && r == "" {
			return nil
		}
		w := &UsageWindow{}
		if u != "" {
			if f, err := strconv.ParseFloat(u, 64); err == nil {
				w.Utilization = f * 100 // headers report 0..1, APIResponse expects 0..100
			}
		}
		if r != "" {
			if sec, err := strconv.ParseInt(r, 10, 64); err == nil {
				w.ResetsAt = time.Unix(sec, 0).UTC().Format(time.RFC3339)
			}
		}
		return w
	}

	resp := &APIResponse{
		FiveHour:       parsePair("5h-utilization", "5h-reset"),
		SevenDay:       parsePair("7d-utilization", "7d-reset"),
		SevenDaySonnet: parsePair("7d_sonnet-utilization", "7d_sonnet-reset"),
		SevenDayOpus:   parsePair("7d_opus-utilization", "7d_opus-reset"),
	}
	if u := get("Overage-Utilization"); u != "" {
		if f, err := strconv.ParseFloat(u, 64); err == nil {
			resp.OverageUtilization = &f
		}
	}
	if v := get("representative-claim"); v != "" {
		resp.RepresentativeClaim = &v
	}
	if v := get("overage-status"); v != "" {
		resp.OverageStatus = &v
	}
	if v := get("overage-disabled-reason"); v != "" {
		resp.OverageDisabledReason = &v
	}
	if resp.FiveHour == nil && resp.SevenDay == nil && resp.SevenDaySonnet == nil && resp.SevenDayOpus == nil &&
		resp.OverageUtilization == nil && resp.RepresentativeClaim == nil && resp.OverageStatus == nil && resp.OverageDisabledReason == nil {
		return nil
	}
	return resp
}

// FetchProfile retrieves user profile info.
func (c *Client) FetchProfile() (map[string]any, error) {
	req, err := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body)}
	}

	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ScopedWindows extracts per-model weekly caps from the limits array. The
// model set is whatever Anthropic reports — currently Fable — so no model
// name is hardcoded here.
func ScopedWindows(limits []LimitEntry) []map[string]any {
	var out []map[string]any
	for _, l := range limits {
		if l.Group != "weekly" || l.Scope == nil || l.Scope.Model == nil {
			continue
		}
		name := l.Scope.Model.DisplayName
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"model": name,
			"pct":   int(l.Percent + 0.5),
			"reset": l.ResetsAt,
		})
	}
	return out
}

// ActiveSeverity returns the severity of the limit entry Anthropic flags as
// is_active — the window currently binding the rate limit — or "" if none is
// active. Only populated when limits[] is present, i.e. from /api/oauth/usage.
func ActiveSeverity(limits []LimitEntry) string {
	for _, l := range limits {
		if l.IsActive {
			return l.Severity
		}
	}
	return ""
}

// minorUnitsToWhole converts an integer minor-unit amount (e.g. cents) to a
// whole currency value using the reported exponent rather than assuming 100.
func minorUnitsToWhole(amountMinor float64, exponent int) float64 {
	return amountMinor / math.Pow(10, float64(exponent))
}

// TransformUsage converts an APIResponse into the flat map[string]any format
// used by the dashboard, hooks, and statusline cache.
func TransformUsage(resp *APIResponse) map[string]any {
	usage := map[string]any{
		"fetched_at": time.Now().Unix(),
	}

	scoped := ScopedWindows(resp.Limits)
	if len(scoped) > 0 {
		usage["scoped"] = scoped
	}
	findScoped := func(name string) map[string]any {
		for _, s := range scoped {
			if m, ok := s["model"].(string); ok && strings.EqualFold(m, name) {
				return s
			}
		}
		return nil
	}

	if resp.FiveHour != nil {
		usage["pct5hr"] = int(resp.FiveHour.Utilization + 0.5)
		usage["reset5hr"] = resp.FiveHour.ResetsAt
	} else {
		usage["pct5hr"] = -1
	}
	if resp.SevenDay != nil {
		usage["pctWeekly"] = int(resp.SevenDay.Utilization + 0.5)
		usage["resetWeekly"] = resp.SevenDay.ResetsAt
	} else {
		usage["pctWeekly"] = -1
	}
	if resp.SevenDaySonnet != nil {
		usage["pctSonnet"] = int(resp.SevenDaySonnet.Utilization + 0.5)
		usage["resetSonnet"] = resp.SevenDaySonnet.ResetsAt
	} else if s := findScoped("Sonnet"); s != nil {
		usage["pctSonnet"] = s["pct"]
		usage["resetSonnet"] = s["reset"]
	} else {
		usage["pctSonnet"] = -1
	}
	if resp.SevenDayOpus != nil {
		usage["pctOpus"] = int(resp.SevenDayOpus.Utilization + 0.5)
		usage["resetOpus"] = resp.SevenDayOpus.ResetsAt
	} else if s := findScoped("Opus"); s != nil {
		usage["pctOpus"] = s["pct"]
		usage["resetOpus"] = s["reset"]
	}
	if resp.SevenDayOauthApps != nil {
		usage["pctOauthApps"] = int(resp.SevenDayOauthApps.Utilization + 0.5)
		usage["resetOauthApps"] = resp.SevenDayOauthApps.ResetsAt
	}
	if resp.SevenDayCowork != nil {
		usage["pctCowork"] = int(resp.SevenDayCowork.Utilization + 0.5)
		usage["resetCowork"] = resp.SevenDayCowork.ResetsAt
	}
	if resp.ExtraUsage != nil {
		eu := map[string]any{
			"is_enabled":   resp.ExtraUsage.IsEnabled,
			"used_credits": resp.ExtraUsage.UsedCredits / 100, // API returns cents
		}
		if resp.ExtraUsage.MonthlyLimit != nil {
			eu["monthly_limit"] = *resp.ExtraUsage.MonthlyLimit / 100
		}
		if resp.ExtraUsage.Utilization != nil {
			eu["utilization"] = *resp.ExtraUsage.Utilization
		}
		usage["extra_usage"] = eu
	}
	if resp.Spend != nil {
		spend := map[string]any{
			"percent": resp.Spend.Percent,
			"enabled": resp.Spend.Enabled,
		}
		if resp.Spend.Used != nil {
			spend["used"] = minorUnitsToWhole(resp.Spend.Used.AmountMinor, resp.Spend.Used.Exponent)
		}
		if resp.Spend.Limit != nil {
			spend["limit"] = minorUnitsToWhole(resp.Spend.Limit.AmountMinor, resp.Spend.Limit.Exponent)
		}
		if resp.Spend.DisabledReason != nil {
			spend["disabled_reason"] = *resp.Spend.DisabledReason
		}
		usage["spend"] = spend
		usage["spend_fetched_at"] = time.Now().Unix()
	}
	if resp.RepresentativeClaim != nil {
		usage["representativeClaim"] = *resp.RepresentativeClaim
	}
	if sev := ActiveSeverity(resp.Limits); sev != "" {
		usage["severity"] = sev
	}
	if resp.OverageStatus != nil {
		usage["overageStatus"] = *resp.OverageStatus
	}
	if resp.OverageDisabledReason != nil {
		usage["overageDisabledReason"] = *resp.OverageDisabledReason
	}

	return usage
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "periscope-telemetry")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
}

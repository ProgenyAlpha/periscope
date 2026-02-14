package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// APIResponse represents the usage API response structure.
type APIResponse struct {
	FiveHour          *UsageWindow `json:"five_hour"`
	SevenDay          *UsageWindow `json:"seven_day"`
	SevenDaySonnet    *UsageWindow `json:"seven_day_sonnet"`
	SevenDayOpus      *UsageWindow `json:"seven_day_opus"`
	SevenDayOauthApps *UsageWindow `json:"seven_day_oauth_apps"`
	SevenDayCowork    *UsageWindow `json:"seven_day_cowork"`
	ExtraUsage *struct {
		IsEnabled    bool     `json:"is_enabled"`
		MonthlyLimit *float64 `json:"monthly_limit"`
		UsedCredits  float64  `json:"used_credits"`
		Utilization  *float64 `json:"utilization"`
	} `json:"extra_usage"`
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
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
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

// FetchProfile retrieves user profile info.
func (c *Client) FetchProfile() (map[string]any, error) {
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/profile", nil)
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

// TransformUsage converts an APIResponse into the flat map[string]any format
// used by the dashboard, hooks, and statusline cache.
func TransformUsage(resp *APIResponse) map[string]any {
	usage := map[string]any{
		"fetched_at": time.Now().Unix(),
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
	} else {
		usage["pctSonnet"] = -1
	}
	if resp.SevenDayOpus != nil {
		usage["pctOpus"] = int(resp.SevenDayOpus.Utilization + 0.5)
		usage["resetOpus"] = resp.SevenDayOpus.ResetsAt
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

	return usage
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "periscope-telemetry")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
}

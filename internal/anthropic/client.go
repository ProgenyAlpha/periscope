package anthropic

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

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
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(body))
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
		return nil, fmt.Errorf("API returned %d", resp.StatusCode)
	}

	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("User-Agent", "periscope-telemetry")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
}

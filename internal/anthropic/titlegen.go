package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GenerateTitle calls Haiku to produce a concise dashboard title from user prompts.
// Returns the title string or an error.
func GenerateTitle(client *Client, project string, prompts []string) (string, error) {
	// Build numbered prompt list with project context
	var userContent strings.Builder
	if project != "" {
		fmt.Fprintf(&userContent, "Project: %s\n", project)
	}
	for i, p := range prompts {
		fmt.Fprintf(&userContent, "%d. %s\n", i+1, p)
	}

	reqBody := map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 30,
		"system":     "Generate a concise dashboard title for this coding session. Format: 'ProjectName: what was done' (e.g. 'Periscope: PWA + push notifications'). Max 8 words. Return ONLY the title, no quotes or explanation.",
		"messages": []map[string]any{
			{"role": "user", "content": userContent.String()},
		},
	}
	bodyBytes, _ := json.Marshal(reqBody)

	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+client.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "periscope-title-gen")

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("title API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(respBody, &apiResp) != nil || len(apiResp.Content) == 0 {
		return "", fmt.Errorf("failed to parse title API response")
	}

	title := strings.TrimSpace(apiResp.Content[0].Text)
	if title == "" {
		return "", fmt.Errorf("empty title returned from API")
	}
	return title, nil
}

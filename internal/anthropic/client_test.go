package anthropic

import (
	"fmt"
	"testing"
)

func ptr(f float64) *float64 { return &f }

func TestTransformUsage(t *testing.T) {
	tests := []struct {
		name   string
		resp   *APIResponse
		checks map[string]any
	}{
		{
			name: "all fields present",
			resp: &APIResponse{
				FiveHour:          &UsageWindow{Utilization: 42.0, ResetsAt: "2026-02-14T12:00:00Z"},
				SevenDay:          &UsageWindow{Utilization: 80.0, ResetsAt: "2026-02-20T00:00:00Z"},
				SevenDaySonnet:    &UsageWindow{Utilization: 33.0, ResetsAt: "2026-02-20T00:00:00Z"},
				SevenDayOpus:      &UsageWindow{Utilization: 91.0, ResetsAt: "2026-02-20T00:00:00Z"},
				SevenDayOauthApps: &UsageWindow{Utilization: 15.0, ResetsAt: "2026-02-20T00:00:00Z"},
				SevenDayCowork:    &UsageWindow{Utilization: 5.0, ResetsAt: "2026-02-20T00:00:00Z"},
				ExtraUsage: &struct {
					IsEnabled    bool     `json:"is_enabled"`
					MonthlyLimit *float64 `json:"monthly_limit"`
					UsedCredits  float64  `json:"used_credits"`
					Utilization  *float64 `json:"utilization"`
				}{
					IsEnabled:    true,
					MonthlyLimit: ptr(10000),
					UsedCredits:  5500,
					Utilization:  ptr(55.0),
				},
			},
			checks: map[string]any{
				"pct5hr":        42,
				"reset5hr":      "2026-02-14T12:00:00Z",
				"pctWeekly":     80,
				"pctSonnet":     33,
				"pctOpus":       91,
				"pctOauthApps":  15,
				"pctCowork":     5,
			},
		},
		{
			name: "all nil windows",
			resp: &APIResponse{},
			checks: map[string]any{
				"pct5hr":    -1,
				"pctWeekly": -1,
				"pctSonnet": -1,
			},
		},
		{
			name: "rounding 49.4 down",
			resp: &APIResponse{
				FiveHour: &UsageWindow{Utilization: 49.4, ResetsAt: "t"},
			},
			checks: map[string]any{
				"pct5hr": 49,
			},
		},
		{
			name: "rounding 49.5 up",
			resp: &APIResponse{
				FiveHour: &UsageWindow{Utilization: 49.5, ResetsAt: "t"},
			},
			checks: map[string]any{
				"pct5hr": 50,
			},
		},
		{
			name: "optional windows absent when nil",
			resp: &APIResponse{
				FiveHour: &UsageWindow{Utilization: 10, ResetsAt: "t"},
				SevenDay: &UsageWindow{Utilization: 20, ResetsAt: "t"},
			},
			checks: map[string]any{
				"pct5hr":    10,
				"pctWeekly": 20,
				"pctSonnet": -1,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := TransformUsage(tc.resp)

			for key, want := range tc.checks {
				got, ok := result[key]
				if !ok {
					t.Errorf("missing key %q", key)
					continue
				}
				if fmt.Sprintf("%v", got) != fmt.Sprintf("%v", want) {
					t.Errorf("key %q: got %v, want %v", key, got, want)
				}
			}

			// Verify optional windows are absent when nil
			if tc.name == "optional windows absent when nil" {
				for _, key := range []string{"pctOpus", "pctOauthApps", "pctCowork"} {
					if _, ok := result[key]; ok {
						t.Errorf("key %q should be absent when window is nil", key)
					}
				}
			}
		})
	}
}

func TestTransformUsageExtraUsageCents(t *testing.T) {
	resp := &APIResponse{
		ExtraUsage: &struct {
			IsEnabled    bool     `json:"is_enabled"`
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  float64  `json:"used_credits"`
			Utilization  *float64 `json:"utilization"`
		}{
			IsEnabled:    true,
			MonthlyLimit: ptr(10000),
			UsedCredits:  5500,
			Utilization:  ptr(55.0),
		},
	}

	result := TransformUsage(resp)
	eu, ok := result["extra_usage"].(map[string]any)
	if !ok {
		t.Fatal("extra_usage missing or wrong type")
	}

	if got := eu["used_credits"]; got != 55.0 {
		t.Errorf("used_credits: got %v, want 55 (5500 cents / 100)", got)
	}
	if got := eu["monthly_limit"]; got != 100.0 {
		t.Errorf("monthly_limit: got %v, want 100 (10000 cents / 100)", got)
	}
	if got := eu["is_enabled"]; got != true {
		t.Errorf("is_enabled: got %v, want true", got)
	}
	if got := eu["utilization"]; got != 55.0 {
		t.Errorf("utilization: got %v, want 55.0", got)
	}
}

func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"401 APIError", &APIError{StatusCode: 401, Body: "unauthorized"}, true},
		{"429 APIError", &APIError{StatusCode: 429, Body: "rate limited"}, false},
		{"500 APIError", &APIError{StatusCode: 500, Body: "server error"}, false},
		{"nil error", nil, false},
		{"non-APIError", fmt.Errorf("some error"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAuthError(tc.err); got != tc.want {
				t.Errorf("IsAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsRateLimited(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"429", &APIError{StatusCode: 429, Body: "rate limited"}, true},
		{"401", &APIError{StatusCode: 401, Body: "unauthorized"}, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRateLimited(tc.err); got != tc.want {
				t.Errorf("IsRateLimited(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestAPIErrorFormat(t *testing.T) {
	err := &APIError{StatusCode: 401, Body: "unauthorized"}
	want := "API returned 401: unauthorized"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

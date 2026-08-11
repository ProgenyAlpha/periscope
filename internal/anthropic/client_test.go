package anthropic

import (
	"encoding/json"
	"fmt"
	"net/http"
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
				"pct5hr":       42,
				"reset5hr":     "2026-02-14T12:00:00Z",
				"pctWeekly":    80,
				"pctSonnet":    33,
				"pctOpus":      91,
				"pctOauthApps": 15,
				"pctCowork":    5,
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

// Real /api/oauth/usage payload shape as of 2026-08-11: the flat per-model
// windows are null and the only per-model cap lives in limits[].
const scopedPayload = `{
  "five_hour":{"utilization":6.0,"resets_at":"2026-08-11T11:30:00.963597+00:00"},
  "seven_day":{"utilization":26.0,"resets_at":"2026-08-15T09:00:00.963620+00:00"},
  "seven_day_opus":null,
  "seven_day_sonnet":null,
  "limits":[
    {"kind":"session","group":"session","percent":6,"severity":"normal","resets_at":"2026-08-11T11:30:00.963597+00:00","scope":null,"is_active":false},
    {"kind":"weekly_all","group":"weekly","percent":26,"severity":"normal","resets_at":"2026-08-15T09:00:00.963620+00:00","scope":null,"is_active":true},
    {"kind":"weekly_scoped","group":"weekly","percent":10,"severity":"normal","resets_at":"2026-08-15T08:59:59.963815+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}
  ]
}`

func TestTransformUsageScopedLimits(t *testing.T) {
	var resp APIResponse
	if err := json.Unmarshal([]byte(scopedPayload), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := TransformUsage(&resp)

	scoped, ok := usage["scoped"].([]map[string]any)
	if !ok || len(scoped) != 1 {
		t.Fatalf("scoped = %#v, want 1 entry", usage["scoped"])
	}
	if scoped[0]["model"] != "Fable" || scoped[0]["pct"] != 10 {
		t.Errorf("scoped[0] = %#v, want Fable at 10", scoped[0])
	}
	if scoped[0]["reset"] != "2026-08-15T08:59:59.963815+00:00" {
		t.Errorf("scoped reset = %v", scoped[0]["reset"])
	}
	// weekly_all and session carry no model scope and must not leak in.
	if usage["pctSonnet"] != -1 {
		t.Errorf("pctSonnet = %v, want -1 when the API reports no Sonnet cap", usage["pctSonnet"])
	}
	if _, present := usage["pctOpus"]; present {
		t.Errorf("pctOpus present, want omitted when the API reports no Opus cap")
	}
}

func TestScopedWindowsBackfillsFlatFields(t *testing.T) {
	resp := &APIResponse{Limits: []LimitEntry{
		{Kind: "weekly_scoped", Group: "weekly", Percent: 44, ResetsAt: "r1", Scope: &struct {
			Model *struct {
				ID          *string `json:"id"`
				DisplayName string  `json:"display_name"`
			} `json:"model"`
			Surface *string `json:"surface"`
		}{Model: &struct {
			ID          *string `json:"id"`
			DisplayName string  `json:"display_name"`
		}{DisplayName: "Sonnet"}}},
	}}
	usage := TransformUsage(resp)
	if usage["pctSonnet"] != 44 || usage["resetSonnet"] != "r1" {
		t.Errorf("pctSonnet=%v resetSonnet=%v, want 44/r1 from the scoped entry", usage["pctSonnet"], usage["resetSonnet"])
	}
}

// fullOauthPayload is the real /api/oauth/usage response body, captured
// 2026-08-11, after Anthropic added the top-level `spend` object alongside
// `limits[].severity`/`is_active` (unrelated internal-codename fields the
// live response also carries — seven_day_omelette, tangelo, nimbus_quill,
// etc. — are trimmed; APIResponse has no tags for them and json.Unmarshal
// ignores unknown fields either way).
const fullOauthPayload = `{
  "five_hour":{"utilization":9.0,"resets_at":"2026-08-11T11:29:59.177422+00:00"},
  "seven_day":{"utilization":27.0,"resets_at":"2026-08-15T08:59:59.177447+00:00"},
  "seven_day_oauth_apps":null,
  "seven_day_opus":null,
  "seven_day_sonnet":null,
  "seven_day_cowork":null,
  "extra_usage":{
    "is_enabled":false,
    "monthly_limit":5500,
    "used_credits":0.0,
    "utilization":0.0,
    "disabled_reason":"out_of_credits"
  },
  "limits":[
    {"kind":"session","group":"session","percent":9,"severity":"normal","resets_at":"2026-08-11T11:29:59.177422+00:00","scope":null,"is_active":false},
    {"kind":"weekly_all","group":"weekly","percent":27,"severity":"normal","resets_at":"2026-08-15T08:59:59.177447+00:00","scope":null,"is_active":true},
    {"kind":"weekly_scoped","group":"weekly","percent":10,"severity":"normal","resets_at":"2026-08-15T09:00:00.177695+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false}
  ],
  "spend":{
    "used":{"amount_minor":0,"currency":"USD","exponent":2},
    "limit":{"amount_minor":5500,"currency":"USD","exponent":2},
    "percent":0,
    "severity":"normal",
    "enabled":false,
    "disabled_reason":"out_of_credits"
  }
}`

// TestTransformUsageFullOauthPayload is both the new-surface test (spend,
// severity) and the regression test: pct5hr/pctWeekly/scoped/extra_usage
// must keep their exact pre-existing names and shapes alongside the new keys.
func TestTransformUsageFullOauthPayload(t *testing.T) {
	var resp APIResponse
	if err := json.Unmarshal([]byte(fullOauthPayload), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := TransformUsage(&resp)

	// Regression: pre-existing keys, unchanged names and shapes.
	if usage["pct5hr"] != 9 {
		t.Errorf("pct5hr = %v, want 9", usage["pct5hr"])
	}
	if usage["pctWeekly"] != 27 {
		t.Errorf("pctWeekly = %v, want 27", usage["pctWeekly"])
	}
	scoped, ok := usage["scoped"].([]map[string]any)
	if !ok || len(scoped) != 1 || scoped[0]["model"] != "Fable" || scoped[0]["pct"] != 10 {
		t.Errorf("scoped = %#v, want 1 entry Fable/10", usage["scoped"])
	}
	eu, ok := usage["extra_usage"].(map[string]any)
	if !ok {
		t.Fatal("extra_usage missing or wrong type")
	}
	if eu["is_enabled"] != false || eu["used_credits"] != 0.0 || eu["monthly_limit"] != 55.0 {
		t.Errorf("extra_usage = %#v, want is_enabled=false used_credits=0 monthly_limit=55", eu)
	}

	// New: severity of the is_active limit entry (weekly_all here).
	if usage["severity"] != "normal" {
		t.Errorf("severity = %v, want normal", usage["severity"])
	}

	// New: spend, minor units converted via the reported exponent (2 here:
	// 0/5500 minor units -> 0/55 whole dollars).
	spend, ok := usage["spend"].(map[string]any)
	if !ok {
		t.Fatal("spend missing or wrong type")
	}
	if spend["used"] != 0.0 {
		t.Errorf("spend.used = %v, want 0", spend["used"])
	}
	if spend["limit"] != 55.0 {
		t.Errorf("spend.limit = %v, want 55", spend["limit"])
	}
	if spend["percent"] != 0.0 {
		t.Errorf("spend.percent = %v, want 0", spend["percent"])
	}
	if spend["enabled"] != false {
		t.Errorf("spend.enabled = %v, want false", spend["enabled"])
	}
	if spend["disabled_reason"] != "out_of_credits" {
		t.Errorf("spend.disabled_reason = %v, want out_of_credits", spend["disabled_reason"])
	}
	if _, ok := usage["spend_fetched_at"].(int64); !ok {
		t.Errorf("spend_fetched_at missing or wrong type: %#v", usage["spend_fetched_at"])
	}
}

func TestActiveSeverity(t *testing.T) {
	limits := []LimitEntry{
		{Kind: "session", Severity: "normal", IsActive: false},
		{Kind: "weekly_all", Severity: "warning", IsActive: true},
	}
	if got := ActiveSeverity(limits); got != "warning" {
		t.Errorf("ActiveSeverity = %q, want warning", got)
	}
	if got := ActiveSeverity(nil); got != "" {
		t.Errorf("ActiveSeverity(nil) = %q, want empty", got)
	}
}

// TestTransformUsageSpendExponent proves the exponent is read from the
// payload rather than hardcoded to 100: exponent 3 here, so /1000 not /100.
func TestTransformUsageSpendExponent(t *testing.T) {
	payload := `{"spend":{"used":{"amount_minor":12345,"exponent":3},"limit":{"amount_minor":50000,"exponent":3},"percent":25,"enabled":true}}`
	var resp APIResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage := TransformUsage(&resp)
	spend, ok := usage["spend"].(map[string]any)
	if !ok {
		t.Fatal("spend missing or wrong type")
	}
	if spend["used"] != 12.345 {
		t.Errorf("spend.used = %v, want 12.345 (12345 minor / 10^3)", spend["used"])
	}
	if spend["limit"] != 50.0 {
		t.Errorf("spend.limit = %v, want 50 (50000 minor / 10^3)", spend["limit"])
	}
	if _, present := spend["disabled_reason"]; present {
		t.Errorf("disabled_reason should be absent when the payload omits it")
	}
}

// TestParseUnifiedHeadersNewFields uses the exact anthropic-ratelimit-unified-*
// headers captured 2026-08-11 from a live /v1/messages ping.
func TestParseUnifiedHeadersNewFields(t *testing.T) {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.09")
	h.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1786447800")
	h.Set("Anthropic-Ratelimit-Unified-7d-Utilization", "0.27")
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", "1786784400")
	h.Set("Anthropic-Ratelimit-Unified-Representative-Claim", "five_hour")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Status", "rejected")
	h.Set("Anthropic-Ratelimit-Unified-Overage-Disabled-Reason", "out_of_credits")

	resp := parseUnifiedHeaders(h)
	if resp == nil {
		t.Fatal("parseUnifiedHeaders returned nil")
	}
	if resp.RepresentativeClaim == nil || *resp.RepresentativeClaim != "five_hour" {
		t.Errorf("RepresentativeClaim = %v, want five_hour", resp.RepresentativeClaim)
	}
	if resp.OverageStatus == nil || *resp.OverageStatus != "rejected" {
		t.Errorf("OverageStatus = %v, want rejected", resp.OverageStatus)
	}
	if resp.OverageDisabledReason == nil || *resp.OverageDisabledReason != "out_of_credits" {
		t.Errorf("OverageDisabledReason = %v, want out_of_credits", resp.OverageDisabledReason)
	}

	usage := TransformUsage(resp)
	if usage["representativeClaim"] != "five_hour" {
		t.Errorf("representativeClaim = %v, want five_hour", usage["representativeClaim"])
	}
	if usage["overageStatus"] != "rejected" {
		t.Errorf("overageStatus = %v, want rejected", usage["overageStatus"])
	}
	if usage["overageDisabledReason"] != "out_of_credits" {
		t.Errorf("overageDisabledReason = %v, want out_of_credits", usage["overageDisabledReason"])
	}
}

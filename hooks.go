package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// --- Hook Payload (from Claude's hook system via stdin) ---

type HookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// --- Transcript Entry ---

type TranscriptEntry struct {
	Type    string           `json:"type"`
	Message *TranscriptMsg   `json:"message,omitempty"`
}

type TranscriptMsg struct {
	Model   string          `json:"model"`
	Usage   *TokenUsage     `json:"usage,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type TokenUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// --- Sidecar State ---

type SidecarState struct {
	LastOffset int64       `json:"lastOffset"`
	Project    string      `json:"project,omitempty"`
	Cumulative *Cumulative `json:"cumulative"`
	LastTurn   *LastTurn   `json:"lastTurn"`
}

type Cumulative struct {
	Input      int64              `json:"input"`
	CacheRead  int64              `json:"cache_read"`
	CacheWrite int64              `json:"cache_write"`
	Output     int64              `json:"output"`
	Cost       float64            `json:"cost"`
	AgentCost  float64            `json:"agent_cost"`
	ToolCost   float64            `json:"tool_cost"`
	ChatCost   float64            `json:"chat_cost"`
	AgentCalls int                `json:"agent_calls"`
	ToolCalls  int                `json:"tool_calls"`
	ChatCalls  int                `json:"chat_calls"`
	Tools      map[string]*ToolStat `json:"tools"`
}

type ToolStat struct {
	Calls    int     `json:"calls"`
	Weighted float64 `json:"weighted"`
}

type LastTurn struct {
	Cost       float64  `json:"cost"`
	Type       string   `json:"type"`
	Model      string   `json:"model"`
	Input      int64    `json:"input"`
	CacheRead  int64    `json:"cache_read"`
	CacheWrite int64    `json:"cache_write"`
	Output     int64    `json:"output"`
	Tools      []string `json:"tools"`
}

// --- Model Pricing ---

type ModelRates struct {
	Input      float64
	CacheRead  float64
	CacheWrite float64
	Output     float64
}

var modelPricing = map[string]ModelRates{
	"claude-opus-4-6":            {5, 0.50, 6.25, 25},
	"claude-opus-4-5":            {5, 0.50, 6.25, 25},
	"claude-opus-4-1":            {15, 1.50, 18.75, 75},
	"claude-sonnet-4-5-20250929": {3, 0.30, 3.75, 15},
	"claude-haiku-4-5-20251001":  {1, 0.10, 1.25, 5},
	"claude-haiku-3-5":           {0.80, 0.08, 1.00, 4},
}

// Token weights for rate limit calculation
var tokenWeights = struct {
	Input, CacheRead, CacheWrite, Output float64
}{1.0, 0, 1.0, 5.0}

func getModelRates(model string) ModelRates {
	for prefix, rates := range modelPricing {
		if strings.HasPrefix(model, prefix) {
			return rates
		}
	}
	return modelPricing["claude-opus-4-6"] // default
}

// --- Hook: Stop ---

func hookStop() {
	payload := readHookPayload()
	if payload == nil || payload.TranscriptPath == "" || payload.SessionID == "" {
		return
	}

	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	os.MkdirAll(stateDir, 0755)
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	// Extract project slug from transcript path
	projectSlug := ""
	parentDir := filepath.Dir(payload.TranscriptPath)
	parentName := filepath.Base(parentDir)
	if parentName != "cost-state" {
		projectSlug = parentName
	}

	// Load or init state
	state := loadOrInitState(statePath)

	// Check for compaction (file got smaller)
	fi, err := os.Stat(payload.TranscriptPath)
	if err != nil {
		return
	}
	if state.LastOffset > fi.Size() {
		state.LastOffset = 0
		state.Cumulative = newCumulative()
	}

	// Read new bytes from transcript
	f, err := os.Open(payload.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close()

	f.Seek(state.LastOffset, io.SeekStart)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB line buffer

	var entries []TranscriptEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry TranscriptEntry
		if json.Unmarshal([]byte(line), &entry) == nil {
			entries = append(entries, entry)
		}
	}

	newOffset, _ := f.Seek(0, io.SeekCurrent)

	// Reset last turn
	turn := &LastTurn{Type: "chat"}

	for _, entry := range entries {
		if entry.Type != "assistant" || entry.Message == nil || entry.Message.Usage == nil {
			continue
		}

		usage := entry.Message.Usage
		model := entry.Message.Model
		rates := getModelRates(model)

		cost := float64(usage.InputTokens)*rates.Input/1e6 +
			float64(usage.CacheReadInputTokens)*rates.CacheRead/1e6 +
			float64(usage.CacheCreationInputTokens)*rates.CacheWrite/1e6 +
			float64(usage.OutputTokens)*rates.Output/1e6

		turnInfo := getTurnInfo(entry.Message.Content)

		// Weighted tokens
		weighted := float64(usage.InputTokens)*tokenWeights.Input +
			float64(usage.CacheReadInputTokens)*tokenWeights.CacheRead +
			float64(usage.CacheCreationInputTokens)*tokenWeights.CacheWrite +
			float64(usage.OutputTokens)*tokenWeights.Output

		// Accumulate
		state.Cumulative.Input += usage.InputTokens
		state.Cumulative.CacheRead += usage.CacheReadInputTokens
		state.Cumulative.CacheWrite += usage.CacheCreationInputTokens
		state.Cumulative.Output += usage.OutputTokens
		state.Cumulative.Cost += cost

		switch turnInfo.turnType {
		case "agent":
			state.Cumulative.AgentCost += cost
			state.Cumulative.AgentCalls++
		case "tool":
			state.Cumulative.ToolCost += cost
			state.Cumulative.ToolCalls++
		default:
			state.Cumulative.ChatCost += cost
			state.Cumulative.ChatCalls++
		}

		// Per-tool tracking
		seen := map[string]bool{}
		for _, toolName := range turnInfo.tools {
			tKey := toolName
			if toolName == "Task" && len(turnInfo.agents) > 0 {
				tKey = "Task/" + turnInfo.agents[0]
			}
			if state.Cumulative.Tools[tKey] == nil {
				state.Cumulative.Tools[tKey] = &ToolStat{}
			}
			state.Cumulative.Tools[tKey].Calls++
			seen[tKey] = true
		}
		if len(seen) > 0 {
			perTool := weighted / float64(len(seen))
			for tKey := range seen {
				state.Cumulative.Tools[tKey].Weighted += perTool
			}
		}

		// This turn
		turn.Cost += cost
		turn.Input += usage.InputTokens
		turn.CacheRead += usage.CacheReadInputTokens
		turn.CacheWrite += usage.CacheCreationInputTokens
		turn.Output += usage.OutputTokens
		turn.Tools = append(turn.Tools, turnInfo.tools...)
		turn.Model = model
		if turnInfo.turnType == "agent" {
			turn.Type = "agent"
		} else if turnInfo.turnType == "tool" && turn.Type != "agent" {
			turn.Type = "tool"
		}
	}

	// Update state
	state.LastOffset = newOffset
	state.LastTurn = turn
	if projectSlug != "" {
		state.Project = projectSlug
	}

	// Write sidecar
	data, _ := json.Marshal(state)
	os.WriteFile(statePath, data, 0644)

	// Append to usage-history.jsonl
	totalCalls := state.Cumulative.AgentCalls + state.Cumulative.ToolCalls + state.Cumulative.ChatCalls
	shortSid := payload.SessionID
	if len(shortSid) > 8 {
		shortSid = shortSid[:8]
	}

	historyEntry := map[string]any{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"sid":   shortSid,
		"input": state.Cumulative.Input,
		"cr":    state.Cumulative.CacheRead,
		"cw":    state.Cumulative.CacheWrite,
		"out":   state.Cumulative.Output,
		"cost":  math.Round(state.Cumulative.Cost*100) / 100,
		"turns": totalCalls,
	}
	line, _ := json.Marshal(historyEntry)

	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	if hf, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		hf.Write(append(line, '\n'))
		hf.Close()
	}
}

// --- Hook: Display ---

func hookDisplay() {
	payload := readHookPayload()
	if payload == nil || payload.SessionID == "" {
		return
	}

	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".claude", "hooks", "cost-state")
	claudeDir := filepath.Join(home, ".claude")
	statePath := filepath.Join(stateDir, payload.SessionID+".json")

	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		output := map[string]any{
			"hookSpecificOutput": map[string]any{
				"hookEventName":    "UserPromptSubmit",
				"additionalContext": "TELEMETRY - First turn - tracking starts next message.",
			},
		}
		data, _ := json.Marshal(output)
		fmt.Print(string(data))
		return
	}

	stateData, err := os.ReadFile(statePath)
	if err != nil {
		return
	}
	stateData = stripBOM(stateData)

	var state SidecarState
	if json.Unmarshal(stateData, &state) != nil || state.Cumulative == nil {
		return
	}

	c := state.Cumulative
	totalCalls := c.AgentCalls + c.ToolCalls + c.ChatCalls

	// Append history entry (every prompt submission = data point for charts)
	shortSid := payload.SessionID
	if len(shortSid) > 8 {
		shortSid = shortSid[:8]
	}
	historyEntry := map[string]any{
		"ts":    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"sid":   shortSid,
		"input": c.Input,
		"cr":    c.CacheRead,
		"cw":    c.CacheWrite,
		"out":   c.Output,
		"cost":  math.Round(c.Cost*100) / 100,
		"turns": totalCalls,
	}
	histLine, _ := json.Marshal(historyEntry)
	histPath := filepath.Join(stateDir, "usage-history.jsonl")
	if hf, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		hf.Write(append(histLine, '\n'))
		hf.Close()
	}

	// Cache hit rate
	totalIn := c.Input + c.CacheRead
	cacheHit := 0.0
	if totalIn > 0 {
		cacheHit = float64(c.CacheRead) / float64(totalIn) * 100
	}
	cacheStr := fmt.Sprintf("%.1f%%", cacheHit)
	if cacheHit < 95 {
		cacheStr = fmt.Sprintf("%.0f%%", cacheHit)
	}

	// Top tools
	toolStr := ""
	if len(c.Tools) > 0 {
		type toolEntry struct {
			name     string
			calls    int
			weighted float64
		}
		var tools []toolEntry
		for name, stat := range c.Tools {
			tools = append(tools, toolEntry{name, stat.Calls, stat.Weighted})
		}
		for i := 0; i < len(tools); i++ {
			for j := i + 1; j < len(tools); j++ {
				if tools[j].weighted > tools[i].weighted {
					tools[i], tools[j] = tools[j], tools[i]
				}
			}
		}
		var parts []string
		limit := 5
		if len(tools) < limit {
			limit = len(tools)
		}
		for _, t := range tools[:limit] {
			w := fmtTokens(t.weighted)
			parts = append(parts, fmt.Sprintf("%s:%dx(%s)", t.name, t.calls, w))
		}
		toolStr = strings.Join(parts, " | ")
	}

	// Refresh usage cache if stale (>30s), fetch from API if needed
	usage := hookRefreshUsage(stateDir, claudeDir)

	// Rate limits + extra usage
	rateStr := ""
	extraStr := ""
	pct5hr := -1
	pctWk := -1
	if usage != nil {
		pct5hr = intOrDefault(usage["pct5hr"], -1)
		pctWk = intOrDefault(usage["pctWeekly"], -1)
		if pct5hr >= 0 {
			rateStr = fmt.Sprintf("5hr [%s] %d%% | Weekly [%s] %d%%",
				progressBar(pct5hr, 20), pct5hr, progressBar(pctWk, 20), pctWk)
		}
		if eu, ok := usage["extra_usage"].(map[string]any); ok {
			enabled, _ := eu["is_enabled"].(bool)
			if enabled {
				used, _ := eu["used_credits"].(float64)
				lim, _ := eu["monthly_limit"].(float64)
				extraStr = fmt.Sprintf("Extra usage: ON ($%.2f/$%.2f)", used, lim)
			} else {
				extraStr = "Extra usage: OFF"
			}
		}
	}

	// Record limit-history snapshot (throttled to 5min intervals)
	if usage != nil && pct5hr >= 0 {
		hookRecordSnapshot(stateDir, usage)
	}

	// Build forecast projection
	forecastStr := ""
	if usage != nil && pct5hr >= 0 {
		forecastStr = hookBuildForecast(stateDir, usage)
	}

	// Build output
	line1 := fmt.Sprintf("TELEMETRY: %d calls (agent:%d tool:%d chat:%d) | cache:%s",
		totalCalls, c.AgentCalls, c.ToolCalls, c.ChatCalls, cacheStr)
	lines := []string{line1}
	if toolStr != "" {
		lines = append(lines, "Tools: "+toolStr)
	}
	if rateStr != "" {
		lines = append(lines, "Rate limits: "+rateStr)
	}
	if forecastStr != "" {
		lines = append(lines, "Forecast: "+forecastStr)
	}
	if extraStr != "" {
		lines = append(lines, extraStr)
	}

	output := map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":    "UserPromptSubmit",
			"additionalContext": strings.Join(lines, "\n"),
		},
	}
	data, _ := json.Marshal(output)
	fmt.Print(string(data))
}

// hookRefreshUsage returns current usage data, refreshing from the Anthropic API if the
// cache is stale (>30s). This makes the hook self-sufficient even when the periscope server
// isn't running.
func hookRefreshUsage(stateDir, claudeDir string) map[string]any {
	cachePath := filepath.Join(stateDir, "usage-api-cache.json")

	// Read existing cache
	cached := readUsageCache(cachePath)
	if cached != nil {
		if fetched, ok := cached["fetched_at"].(float64); ok {
			if time.Since(time.Unix(int64(fetched), 0)) < 30*time.Second {
				return cached // Fresh enough
			}
		}
	}

	// Need refresh — read OAuth token
	credPath := filepath.Join(claudeDir, ".credentials.json")
	credData, err := os.ReadFile(credPath)
	if err != nil {
		return cached // No creds, use stale cache
	}
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(credData, &creds) != nil || creds.ClaudeAiOauth.AccessToken == "" {
		return cached
	}

	// Fetch from Anthropic API (3s timeout — acceptable in UserPromptSubmit hook)
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/api/oauth/usage", nil)
	req.Header.Set("Authorization", "Bearer "+creds.ClaudeAiOauth.AccessToken)
	req.Header.Set("User-Agent", "periscope-hook")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return cached
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return cached
	}

	// Parse — same structure as store.go fetchUsage
	var apiResp struct {
		FiveHour       *usageWindow `json:"five_hour"`
		SevenDay       *usageWindow `json:"seven_day"`
		SevenDaySonnet *usageWindow `json:"seven_day_sonnet"`
		ExtraUsage     *struct {
			IsEnabled    bool     `json:"is_enabled"`
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  float64  `json:"used_credits"`
		} `json:"extra_usage"`
	}
	if json.Unmarshal(body, &apiResp) != nil {
		return cached
	}

	usage := map[string]any{
		"fetched_at": time.Now().Unix(),
	}
	if apiResp.FiveHour != nil {
		usage["pct5hr"] = int(apiResp.FiveHour.Utilization + 0.5)
		usage["reset5hr"] = apiResp.FiveHour.ResetsAt
	} else {
		usage["pct5hr"] = -1
	}
	if apiResp.SevenDay != nil {
		usage["pctWeekly"] = int(apiResp.SevenDay.Utilization + 0.5)
		usage["resetWeekly"] = apiResp.SevenDay.ResetsAt
	} else {
		usage["pctWeekly"] = -1
	}
	if apiResp.SevenDaySonnet != nil {
		usage["pctSonnet"] = int(apiResp.SevenDaySonnet.Utilization + 0.5)
	} else {
		usage["pctSonnet"] = -1
	}
	if apiResp.ExtraUsage != nil {
		eu := map[string]any{
			"is_enabled":   apiResp.ExtraUsage.IsEnabled,
			"used_credits": apiResp.ExtraUsage.UsedCredits / 100,
		}
		if apiResp.ExtraUsage.MonthlyLimit != nil {
			eu["monthly_limit"] = *apiResp.ExtraUsage.MonthlyLimit / 100
		}
		usage["extra_usage"] = eu
	}

	// Write cache file (same format the server uses)
	result, _ := json.Marshal(usage)
	os.WriteFile(cachePath, result, 0644)

	return usage
}

func readUsageCache(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	data = stripBOM(data)
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// hookRecordSnapshot appends a limit-history entry if >5min since the last one.
// This keeps the forecast fed with data points even when the periscope server isn't running.
func hookRecordSnapshot(stateDir string, usage map[string]any) {
	histPath := filepath.Join(stateDir, "limit-history.jsonl")

	// Check last entry time — throttle to 5min
	if f, err := os.Open(histPath); err == nil {
		fi, _ := f.Stat()
		offset := fi.Size() - 512
		if offset < 0 {
			offset = 0
		}
		f.Seek(offset, io.SeekStart)
		scanner := bufio.NewScanner(f)
		var lastLine string
		for scanner.Scan() {
			if l := strings.TrimSpace(scanner.Text()); l != "" {
				lastLine = l
			}
		}
		f.Close()

		if lastLine != "" {
			var entry map[string]any
			if json.Unmarshal([]byte(lastLine), &entry) == nil {
				if ts, ok := entry["ts"].(string); ok {
					if t, err := time.Parse(time.RFC3339, ts); err == nil {
						if time.Since(t) < 5*time.Minute {
							return
						}
					}
				}
			}
		}
	}

	// Build snapshot
	snap := map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"pct5hr":    intOrDefault(usage["pct5hr"], -1),
		"pctWeekly": intOrDefault(usage["pctWeekly"], -1),
	}
	if v, ok := usage["reset5hr"].(string); ok {
		snap["reset5hr"] = v
	}
	if v, ok := usage["resetWeekly"].(string); ok {
		snap["resetWeekly"] = v
	}

	line, _ := json.Marshal(snap)
	if f, err := os.OpenFile(histPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
}

// hookBuildForecast calculates projected usage at reset time.
// 5hr window: rate-based (blended current 30min rate + average rate).
// Weekly window: duty-cycle adjusted burn rate (stable across sleep/wake cycles).
func hookBuildForecast(stateDir string, usage map[string]any) string {
	histPath := filepath.Join(stateDir, "limit-history.jsonl")

	data, err := os.ReadFile(histPath)
	if err != nil {
		return ""
	}

	// Parse last 60 entries
	allLines := strings.Split(strings.TrimSpace(string(data)), "\n")
	start := len(allLines) - 60
	if start < 0 {
		start = 0
	}

	type limitPoint struct {
		ts     time.Time
		pct5hr float64
		pctWk  float64
	}

	var pts []limitPoint
	for _, line := range allLines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		tsStr, ok := entry["ts"].(string)
		if !ok {
			continue
		}
		t, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			continue
		}
		pts = append(pts, limitPoint{
			ts:     t,
			pct5hr: floatOrDefault(entry["pct5hr"], -1),
			pctWk:  floatOrDefault(entry["pctWeekly"], -1),
		})
	}

	if len(pts) < 3 {
		return ""
	}

	now := time.Now().UTC()
	pct5hr := intOrDefault(usage["pct5hr"], -1)
	pctWk := intOrDefault(usage["pctWeekly"], -1)
	reset5hr, _ := usage["reset5hr"].(string)
	resetWk, _ := usage["resetWeekly"].(string)

	// Load duty cycle data (written by dashboard for weekly projection stability)
	dutyHrs := 24.0
	dutyCachePath := filepath.Join(stateDir, "duty-cache.json")
	if dcData, err := os.ReadFile(dutyCachePath); err == nil {
		var dc map[string]any
		if json.Unmarshal(dcData, &dc) == nil {
			if updAt, ok := dc["updated_at"].(float64); ok {
				if time.Since(time.Unix(int64(updAt), 0)) < time.Hour {
					if dh, ok := dc["activeHrsPerDay"].(float64); ok && dh > 0 {
						dutyHrs = dh
					}
				}
			}
		}
	}

	type windowSpec struct {
		label, resetStr string
		current         int
		use5hr          bool // true = 5hr rate-based, false = weekly duty-adjusted
	}
	windows := []windowSpec{
		{"5h", reset5hr, pct5hr, true},
		{"wk", resetWk, pctWk, false},
	}

	var parts []string
	for _, w := range windows {
		if w.current < 0 || w.resetStr == "" {
			continue
		}
		resetTime, err := time.Parse(time.RFC3339, w.resetStr)
		if err != nil {
			continue
		}
		hrsLeft := resetTime.Sub(now).Hours()
		if hrsLeft <= 0 {
			continue
		}

		// Build series with reset detection (big drop = window rolled over)
		var series []limitPoint
		prev := -1.0
		for _, pt := range pts {
			v := pt.pct5hr
			if !w.use5hr {
				v = pt.pctWk
			}
			if v < 0 {
				continue
			}
			if prev >= 0 && v < prev-15 {
				series = nil // Reset detected
			}
			series = append(series, pt)
			prev = v
		}
		if len(series) < 2 {
			continue
		}

		var proj int
		var tl, rateStr string

		if !w.use5hr {
			// Weekly: duty-adjusted active-hours projection
			elapsedHrs := 168 - hrsLeft
			activeElapsed := math.Max(0.5, (elapsedHrs/24)*dutyHrs)
			activeRemaining := (hrsLeft / 24) * dutyHrs
			burnRate := float64(w.current) / activeElapsed
			proj = int(math.Round(float64(w.current) + burnRate*activeRemaining))
			tl = fmt.Sprintf("%.1fd", hrsLeft/24)
			rateStr = fmt.Sprintf("%.1f%%/ah", burnRate)
		} else {
			// 5hr: current rate (last 30min) blended with average rate
			cutoff := now.Add(-30 * time.Minute)
			var recent []limitPoint
			for _, s := range series {
				if !s.ts.Before(cutoff) {
					recent = append(recent, s)
				}
			}

			curRate := 0.0
			if len(recent) >= 2 {
				first, last := recent[0], recent[len(recent)-1]
				dh := last.ts.Sub(first.ts).Hours()
				if dh > 0.01 {
					curRate = (last.pct5hr - first.pct5hr) / dh
				}
			}

			firstAll, lastAll := series[0], series[len(series)-1]
			dAll := lastAll.ts.Sub(firstAll.ts).Hours()
			avgRate := 0.0
			if dAll > 0.05 {
				avgRate = (lastAll.pct5hr - firstAll.pct5hr) / dAll
			}

			// Weighted blend: 60% current, 40% average
			rate := 0.0
			if curRate > 0 && avgRate > 0 {
				rate = 0.6*curRate + 0.4*avgRate
			} else if curRate > 0 {
				rate = curRate
			} else if avgRate > 0 {
				rate = avgRate
			}

			proj = int(math.Round(float64(w.current) + rate*hrsLeft))
			tl = fmt.Sprintf("%.1fh", hrsLeft)
			rateStr = fmt.Sprintf("%.1f%%/h", rate)
		}

		verdict := "OK"
		if !w.use5hr && proj <= w.current {
			verdict = "idle"
		} else if proj > 100 {
			verdict = "OVER LIMIT"
		} else if proj > 90 {
			verdict = "SLOW DOWN"
		} else if proj > 70 {
			verdict = "monitor"
		}

		parts = append(parts, fmt.Sprintf("%s:%d%%->~%d%%(%s left, %s) %s",
			w.label, w.current, proj, tl, rateStr, verdict))
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " | ")
}

func floatOrDefault(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return def
}

// --- Helpers ---

func readHookPayload() *HookPayload {
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return nil
	}
	var p HookPayload
	if json.Unmarshal(data, &p) != nil {
		return nil
	}
	return &p
}

func loadOrInitState(path string) *SidecarState {
	if data, err := os.ReadFile(path); err == nil {
		data = stripBOM(data)
		var state SidecarState
		if json.Unmarshal(data, &state) == nil {
			if state.Cumulative == nil {
				state.Cumulative = newCumulative()
			}
			if state.Cumulative.Tools == nil {
				state.Cumulative.Tools = map[string]*ToolStat{}
			}
			return &state
		}
	}
	return &SidecarState{
		Cumulative: newCumulative(),
		LastTurn:   &LastTurn{Type: "chat"},
	}
}

func newCumulative() *Cumulative {
	return &Cumulative{Tools: map[string]*ToolStat{}}
}

type turnInfo struct {
	turnType string
	tools    []string
	agents   []string
}

func getTurnInfo(content json.RawMessage) turnInfo {
	info := turnInfo{turnType: "chat"}
	if content == nil {
		return info
	}
	var blocks []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Input *struct {
			SubagentType string `json:"subagent_type"`
		} `json:"input,omitempty"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return info
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			info.tools = append(info.tools, b.Name)
			if b.Name == "Task" {
				info.turnType = "agent"
				if b.Input != nil && b.Input.SubagentType != "" {
					info.agents = append(info.agents, b.Input.SubagentType)
				}
			} else if info.turnType != "agent" {
				info.turnType = "tool"
			}
		}
	}
	return info
}

func fmtTokens(v float64) string {
	if v >= 1e9 {
		return fmt.Sprintf("%.1fB", v/1e9)
	}
	if v >= 1e6 {
		return fmt.Sprintf("%.1fM", v/1e6)
	}
	if v >= 1e3 {
		return fmt.Sprintf("%.0fK", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func progressBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	filled := pct * width / 100
	if filled > width {
		filled = width
	}
	return strings.Repeat("#", filled) + strings.Repeat("-", width-filled)
}

func intOrDefault(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return def
}

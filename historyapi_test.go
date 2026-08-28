package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

func seedHistoryRows(t *testing.T, app *App, sid string, start time.Time, n int, step time.Duration) {
	t.Helper()
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * step)
		row := fmt.Sprintf(
			`{"cost":%g,"cr":10,"cw":20,"input":30,"out":40,"sid":%q,"ts":%q,"turns":1}`,
			float64(i), sid, ts.UTC().Format(time.RFC3339))
		if _, err := app.DB.Exec("INSERT INTO history(ts, data) VALUES(?, ?)",
			ts.UTC().Format(time.RFC3339), row); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
}

func getJSON(t *testing.T, h http.Handler, path string) (int, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var m map[string]json.RawMessage
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("%s: body is not JSON: %v", path, err)
		}
	}
	return rec.Code, m
}

func historyLen(t *testing.T, m map[string]json.RawMessage, key string) int {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(m[key], &rows); err != nil {
		t.Fatalf("%s is not an array: %v", key, err)
	}
	return len(rows)
}

// The compatibility promise: a client that sends no parameters — every widget
// shipped before this change — gets exactly what it got before.
func TestHandleData_NoParamsStillReturnsEveryHistoryRow(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	seedHistoryRows(t, app, "sessAAAA", time.Now().Add(-300*24*time.Hour), 400, time.Hour)

	code, m := getJSON(t, h, "/api/data")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := historyLen(t, m, "history"); n != 400 {
		t.Fatalf("unparameterised /api/data returned %d history rows, want all 400", n)
	}
}

func TestHandleData_RollupThinsOldRowsAndKeepsRecentOnes(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	// 300 old rows, one every hour, ending 60 days ago.
	seedHistoryRows(t, app, "oldsess1", time.Now().Add(-60*24*time.Hour-300*time.Hour), 300, time.Hour)
	// 50 recent rows inside the full-resolution tier.
	seedHistoryRows(t, app, "newsess1", time.Now().Add(-24*time.Hour), 50, time.Minute)

	_, full := getJSON(t, h, "/api/data")
	fullN := historyLen(t, full, "history")

	code, m := getJSON(t, h, "/api/data?history=rollup")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	n := historyLen(t, m, "history")
	if n >= fullN {
		t.Fatalf("rollup returned %d rows, full returned %d; want a reduction", n, fullN)
	}
	// Every recent row must still be present.
	var rows []map[string]any
	json.Unmarshal(m["history"], &rows)
	recent := 0
	for _, r := range rows {
		if r["sid"] == "newsess1" {
			recent++
		}
	}
	if recent != 50 {
		t.Fatalf("rollup kept %d of 50 rows inside the full-resolution window", recent)
	}
}

func TestHandleData_HistoryNoneDropsTheArrayButKeepsTheKey(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	seedHistoryRows(t, app, "sessAAAA", time.Now().Add(-10*24*time.Hour), 20, time.Hour)

	code, m := getJSON(t, h, "/api/data?history=none")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if _, ok := m["history"]; !ok {
		t.Fatal("history key vanished; the payload shape must stay stable")
	}
	if n := historyLen(t, m, "history"); n != 0 {
		t.Fatalf("history=none returned %d rows, want 0", n)
	}
}

func TestHandleData_RejectsUnknownHistoryMode(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	code, _ := getJSON(t, h, "/api/data?history=banana")
	if code != http.StatusBadRequest {
		t.Fatalf("history=banana -> %d, want 400", code)
	}
}

func TestHandleData_FromToWindow(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	base := time.Now().Add(-20 * 24 * time.Hour).Truncate(time.Hour)
	seedHistoryRows(t, app, "sessAAAA", base, 240, time.Hour)

	from := base.Add(100 * time.Hour)
	to := base.Add(110 * time.Hour)
	q := fmt.Sprintf("/api/data?from=%s&to=%s",
		url.QueryEscape(from.UTC().Format(time.RFC3339)),
		url.QueryEscape(to.UTC().Format(time.RFC3339)))
	code, m := getJSON(t, h, q)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := historyLen(t, m, "history"); n != 11 {
		t.Fatalf("windowed /api/data returned %d rows, want 11", n)
	}
}

// --- /api/history ---

func TestHandleHistory_SingleDayZoomIsFullResolution(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	base := time.Now().Add(-100 * 24 * time.Hour).Truncate(time.Hour)
	// One snapshot a minute for a whole day.
	seedHistoryRows(t, app, "spikeday", base, 1440, time.Minute)

	q := fmt.Sprintf("/api/history?from=%s&to=%s",
		url.QueryEscape(base.UTC().Format(time.RFC3339)),
		url.QueryEscape(base.Add(24*time.Hour).UTC().Format(time.RFC3339)))
	code, m := getJSON(t, h, q)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if n := historyLen(t, m, "history"); n != 1440 {
		t.Fatalf("single-day zoom returned %d rows, want all 1440 at full resolution", n)
	}
}

func TestHandleHistory_AcceptsEpochSeconds(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	base := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Hour)
	seedHistoryRows(t, app, "sessAAAA", base, 60, time.Minute)

	q := fmt.Sprintf("/api/history?from=%d&to=%d",
		base.Unix(), base.Add(time.Hour).Unix())
	code, m := getJSON(t, h, q)
	if code != http.StatusOK {
		t.Fatalf("epoch bounds -> %d, want 200", code)
	}
	if n := historyLen(t, m, "history"); n != 60 {
		t.Fatalf("epoch window returned %d rows, want 60", n)
	}
}

func TestHandleHistory_ExplicitBucketDownsamples(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	base := time.Now().Add(-100 * 24 * time.Hour).Truncate(time.Hour)
	seedHistoryRows(t, app, "sessAAAA", base, 1440, time.Minute)

	q := fmt.Sprintf("/api/history?from=%d&to=%d&bucket=1h", base.Unix(), base.Add(24*time.Hour).Unix())
	code, m := getJSON(t, h, q)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	n := historyLen(t, m, "history")
	if n != 48 {
		t.Fatalf("hourly bucket over 24h of one session returned %d rows, want 48", n)
	}
	var bucket string
	json.Unmarshal(m["bucket"], &bucket)
	if bucket != "1h0m0s" {
		t.Fatalf("echoed bucket = %q", bucket)
	}
}

func TestHandleHistory_RejectsBadInput(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	now := time.Now()

	cases := []struct {
		name string
		path string
	}{
		{"missing from", "/api/history"},
		{"unparseable from", "/api/history?from=yesterday"},
		{"unparseable to", "/api/history?from=" + strconv.FormatInt(now.Add(-time.Hour).Unix(), 10) + "&to=soon"},
		{"to before from", fmt.Sprintf("/api/history?from=%d&to=%d", now.Unix(), now.Add(-time.Hour).Unix())},
		{"to equals from", fmt.Sprintf("/api/history?from=%d&to=%d", now.Unix(), now.Unix())},
		{"negative bucket", fmt.Sprintf("/api/history?from=%d&to=%d&bucket=-1h", now.Add(-time.Hour).Unix(), now.Unix())},
		{"nonsense bucket", fmt.Sprintf("/api/history?from=%d&to=%d&bucket=lots", now.Add(-time.Hour).Unix(), now.Unix())},
		{"sub-second bucket", fmt.Sprintf("/api/history?from=%d&to=%d&bucket=100ms", now.Add(-time.Hour).Unix(), now.Unix())},
		{"span beyond retention", fmt.Sprintf("/api/history?from=%d&to=%d&bucket=24h", now.Add(-900*24*time.Hour).Unix(), now.Unix())},
		{"wide span at full resolution", fmt.Sprintf("/api/history?from=%d&to=%d", now.Add(-90*24*time.Hour).Unix(), now.Unix())},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s -> %d, want 400 (body %q)", tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// A wide span is fine as long as the caller asked for a bucket — that is the
// difference between "downsample this for me" and "scan the whole table".
func TestHandleHistory_WideSpanWithBucketIsAccepted(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	now := time.Now()
	q := fmt.Sprintf("/api/history?from=%d&to=%d&bucket=24h", now.Add(-200*24*time.Hour).Unix(), now.Unix())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, q, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wide bucketed span -> %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleHistory_RejectsNonGET(t *testing.T) {
	app := newTestApp(t, "")
	h := newTestHandler(app)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/history?from=0", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/history -> %d, want 405", rec.Code)
	}
}

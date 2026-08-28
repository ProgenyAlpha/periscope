package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ProgenyAlpha/periscope/internal/store"
)

// Saving the statusline config must work on a fresh install, where
// ~/.claude/statusline/ has never been created. os.WriteFile does not create
// parent directories, so the handler returned 500 and every toggle the user
// made in the settings widget was silently discarded — the statusline kept
// falling back to compiled defaults.
func TestHandleConfig_CreatesMissingDirectory(t *testing.T) {
	claudeDir := t.TempDir()
	db, err := store.OpenDB(filepath.Join(t.TempDir(), "periscope.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := &App{ClaudeDir: claudeDir, DB: db}

	body := `{"segments":{"tools":{"enabled":false}}}`
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handleConfig(app, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	path := filepath.Join(claudeDir, "statusline", "statusline-config.json")
	saved, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("config was not persisted: %v", readErr)
	}

	var got map[string]any
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if _, ok := got["segments"]; !ok {
		t.Errorf("saved config lost its segments: %s", saved)
	}
}

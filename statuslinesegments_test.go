package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The settings widget offers a fixed list of segments. If it drifts from what
// getSegment actually implements, the dashboard either hides a real segment or
// offers one that renders nothing.
func TestSettingsWidgetMatchesImplementedSegments(t *testing.T) {
	src, err := os.ReadFile("statusline.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func getSegment(")
	if start < 0 {
		t.Fatal("getSegment not found")
	}
	end := strings.Index(body[start:], "\n}\n")
	implemented := map[string]bool{}
	for _, m := range regexp.MustCompile(`case "([a-z0-9-]+)":`).FindAllStringSubmatch(body[start:start+end], -1) {
		implemented[m[1]] = true
	}

	w, err := os.ReadFile("defaults/widgets/statusline-settings.html")
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{}
	seg := regexp.MustCompile(`(?m)^    '([a-z0-9-]+)':\s*\{l:`)
	for _, m := range seg.FindAllStringSubmatch(string(w), -1) {
		offered[m[1]] = true
	}

	if len(implemented) == 0 || len(offered) == 0 {
		t.Fatalf("parsed %d implemented, %d offered — parser is broken", len(implemented), len(offered))
	}
	for name := range implemented {
		if !offered[name] {
			t.Errorf("segment %q is implemented but the settings widget does not offer it", name)
		}
	}
	for name := range offered {
		if !implemented[name] {
			t.Errorf("settings widget offers %q but getSegment does not implement it", name)
		}
	}
	if t.Failed() {
		t.Logf("implemented: %v", sortedKeys(implemented))
		t.Logf("offered:     %v", sortedKeys(offered))
	}
}

func sortedKeys(m map[string]bool) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

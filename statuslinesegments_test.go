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

	// Default order and row assignment must match too, or the dashboard shows a
	// layout the statusline does not actually render.
	codeOrder := listFrom(body, `defaultOrder := \[\]string\{([^}]*)\}`)
	widgetOrder := listFrom(string(w), `_dOrder:\[([^\]]*)\]`)
	if strings.Join(codeOrder, ",") != strings.Join(widgetOrder, ",") {
		t.Errorf("default order differs\n  code:   %v\n  widget: %v", codeOrder, widgetOrder)
	}

	codeRow := map[string]string{}
	for _, m := range regexp.MustCompile(`"([a-z0-9-]+)":\s*([12])`).FindAllStringSubmatch(section(body, "defaultRow := map[string]int{", "}"), -1) {
		codeRow[m[1]] = m[2]
	}
	for _, m := range regexp.MustCompile(`(?m)^    '([a-z0-9-]+)':.*dr:([12])`).FindAllStringSubmatch(string(w), -1) {
		if want, ok := codeRow[m[1]]; ok && want != m[2] {
			t.Errorf("segment %q: widget row %s, code row %s", m[1], m[2], want)
		}
	}
}

func section(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

func listFrom(s, pattern string) []string {
	m := regexp.MustCompile(pattern).FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	var out []string
	for _, f := range regexp.MustCompile(`["'\x60]([a-z0-9-]+)["'\x60]`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, f[1])
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var k []string
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

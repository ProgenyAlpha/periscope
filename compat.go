// compat.go — thin adapters between main package and internal/ packages.
// Keeps hooks.go, statusline.go, and installer.go working without rewriting
// every call site for functions that moved to internal/.
package main

import (
	"net/http"
	"time"

	"github.com/shawnwakeman/periscope/internal/store"
)

func stripBOM(data []byte) []byte { return store.StripBOM(data) }

var sidecarExclude = store.SidecarExclude

func httpGet(url string) (*http.Response, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	return client.Get(url)
}
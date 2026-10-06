package web

import (
	"os"
	"strings"
	"testing"
)

func TestConsoleHasAccountHistoryAndResponsiveLandmarks(t *testing.T) {
	page, err := os.ReadFile("index.html")
	if err != nil { t.Fatal(err) }
	for _, required := range []string{"aria-label=\"主导航\"", "id=\"history-list\"", "id=\"account-dialog\"", "@media (max-width: 760px)", "id=\"guest-notice\"", "prefers-reduced-motion"} {
		if !strings.Contains(string(page), required) { t.Errorf("console missing %q", required) }
	}
}

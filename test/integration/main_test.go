//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if offlineMarketRun() {
		os.Exit(m.Run())
	}
	os.Exit(Main(m))
}

func offlineMarketRun() bool {
	if strings.TrimSpace(os.Getenv("SOCIALWEB_IT_OFFLINE")) != "" {
		return true
	}
	pattern := ""
	for _, arg := range os.Args {
		for _, prefix := range []string{"-test.run=", "--test.run="} {
			if strings.HasPrefix(arg, prefix) {
				pattern = strings.TrimPrefix(arg, prefix)
			}
		}
	}
	if pattern == "" {
		return false
	}
	for _, part := range strings.Split(pattern, "|") {
		part = strings.Trim(strings.TrimSpace(part), "^$")
		if part == "" || !strings.HasPrefix(part, "TestMarket") {
			return false
		}
	}
	return true
}

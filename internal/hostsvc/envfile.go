package hostsvc

import (
	"os"
	"strings"
)

func ReadEnvFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			out = append(out, line)
		}
	}
	return out
}

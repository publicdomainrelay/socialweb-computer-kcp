package portscan

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

func cmdlineIn(root string, pid int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, fmt.Errorf("portscan: reading the command line of %d: %w", pid, err)
	}
	return strs.SplitNUL(b), nil
}

func parsePSCommand(out []byte) []string {
	line := strings.TrimRight(string(out), "\n")
	if line == "" {
		return nil
	}
	return []string{line}
}

package portscan

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

func parseLsof(out []byte) (map[int][]int, error) {
	byPort := map[int][]int{}
	pid := 0
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			p, err := strconv.Atoi(line[1:])
			if err != nil {
				return nil, fmt.Errorf("portscan: lsof pid %q: %w", line[1:], err)
			}
			pid = p
		case 'n':
			port, ok := lsofPort(line[1:])
			if !ok || pid == 0 {
				continue
			}
			if !slices.Contains(byPort[port], pid) {
				byPort[port] = append(byPort[port], pid)
			}
		}
	}
	return byPort, nil
}

func lsofPort(name string) (int, bool) {
	i := strings.LastIndexByte(name, ':')
	if i < 0 {
		return 0, false
	}
	port, err := strconv.Atoi(name[i+1:])
	if err != nil {
		return 0, false
	}
	return port, true
}

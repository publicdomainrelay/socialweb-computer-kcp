package memsize

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var sizeRE = regexp.MustCompile(`(?i)^\s*(\d+)\s*([KMGT])?i?B?\s*$`)

func ParseMiB(s string) (int, error) {
	if strings.TrimSpace(s) == "" {
		return 0, nil
	}
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q is not <number>[K|M|G|T][i][B]", s)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, err
	}
	switch strings.ToUpper(m[2]) {
	case "K":
		return max(1, (n+512)/1024), nil
	case "G":
		return n * 1024, nil
	case "T":
		return n * 1024 * 1024, nil
	}
	return n, nil
}

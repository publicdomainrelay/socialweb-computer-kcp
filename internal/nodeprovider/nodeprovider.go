package nodeprovider

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

const HostnameMaxBytes = 64

var ErrBadName = errors.New("nodeprovider: derived node name is not DNS-1123")

func NodeName(clusterLabel, nodeID string) (string, error) {
	if clusterLabel == "" || nodeID == "" {
		return "", fmt.Errorf("%w: cluster and node id are both required", ErrBadName)
	}
	name := strings.ToLower(clusterLabel + "-" + nodeID)
	if len(name) > 253 || !dns1123.MatchString(name) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	if len(name) > HostnameMaxBytes {
		return "", fmt.Errorf("%w: %q is %d bytes, and a market node takes its node name as its hostname, "+
			"which holds at most %d", ErrBadName, name, len(name), HostnameMaxBytes)
	}
	return name, nil
}

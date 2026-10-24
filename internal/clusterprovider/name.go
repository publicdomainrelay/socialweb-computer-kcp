package clusterprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

const InstancePrefix = "pdr-"

const MaxNameLen = 63

var label = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func InstanceName(cluster string) (string, error) {
	if cluster == "" {
		return "", errors.New("clusterprovider: a Cluster CR with no name has no instance name")
	}
	base := strings.ReplaceAll(cluster, ".", "-")
	if !label.MatchString(base) {
		return "", fmt.Errorf(
			"clusterprovider: %q is not usable as an instance name even after normalisation "+
				"(dots become dashes, and a DNS-1123 label is [a-z0-9-], starting and ending "+
				"alphanumeric)", cluster)
	}
	if len(InstancePrefix)+len(base) <= MaxNameLen {
		return InstancePrefix + base, nil
	}
	sum := sha256.Sum256([]byte(cluster))
	suffix := "-" + hex.EncodeToString(sum[:])[:8]
	keep := MaxNameLen - len(InstancePrefix) - len(suffix)
	return InstancePrefix + strings.TrimRight(base[:keep], "-") + suffix, nil
}

func ClusterDataDir(stateDir, cluster string) (string, error) {
	name, err := InstanceName(cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, name), nil
}

const NodesDirName = "nodes"

func NodesDir(stateDir, cluster string) string {
	return filepath.Join(stateDir, cluster, NodesDirName)
}

func NodeDataDir(stateDir, cluster, nodeName string) string {
	return filepath.Join(NodesDir(stateDir, cluster), nodeName)
}

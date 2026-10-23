package clusterprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
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

const (
	rootWorkspace = "root"

	childLabel = "clusters"

	ownersGroup = "socialweb.computer:cluster-owners"
)

func accountHash(did string) string {
	sum := sha256.Sum256([]byte(did))
	return new(big.Int).SetBytes(sum[:]).Text(36)[:8]
}

func mountPath(did, cluster string) string {
	return rootWorkspace + ":" + accountHash(did) + ":" + cluster + ":" + childLabel
}

var clusterKeyShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func clusterKey(home string) (string, error) {
	parts := strings.Split(home, ":")
	if len(parts) != 3 || parts[0] != rootWorkspace || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf(
			"kcpclient: %q is not a home workspace path of the form %s:<account>:<cluster>, so the "+
				"key a cluster is known by inside a provider cannot be derived from it",
			home, rootWorkspace)
	}
	key := parts[1] + "-" + parts[2]
	if !clusterKeyShape.MatchString(key) {
		return "", fmt.Errorf(
			"kcpclient: the home workspace path %q resolves to %q, which is not a DNS-1123 label: "+
				"the key names a state dir, a port lease, a node and a mint credential, so a "+
				"character outside [a-z0-9-] would make it name nothing those can hold", home, key)
	}
	return key, nil
}

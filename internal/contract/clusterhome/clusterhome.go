package clusterhome

import (
	"crypto/sha256"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

const (
	RootWorkspace = "root"

	ChildLabel = "clusters"

	OwnersGroup = "socialweb.computer:cluster-owners"
)

func AccountHash(did string) string {
	sum := sha256.Sum256([]byte(did))
	return new(big.Int).SetBytes(sum[:]).Text(36)[:8]
}

func AccountPath(did string) string {
	return RootWorkspace + ":" + AccountHash(did)
}

func HomePath(did, cluster string) string {
	return AccountPath(did) + ":" + cluster
}

func MountPath(did, cluster string) string {
	return HomePath(did, cluster) + ":" + ChildLabel
}

var keyShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func Key(home string) (string, error) {
	parts := strings.Split(home, ":")
	if len(parts) != 3 || parts[0] != RootWorkspace || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf(
			"clusterhome: %q is not a home workspace path of the form %s:<account>:<cluster>, so the "+
				"key a cluster is known by inside a provider cannot be derived from it",
			home, RootWorkspace)
	}
	key := parts[1] + "-" + parts[2]
	if !keyShape.MatchString(key) {
		return "", fmt.Errorf(
			"clusterhome: the home workspace path %q resolves to %q, which is not a DNS-1123 label: "+
				"the key names a state dir, a port lease, a node and a mint credential, so a "+
				"character outside [a-z0-9-] would make it name nothing those can hold", home, key)
	}
	return key, nil
}

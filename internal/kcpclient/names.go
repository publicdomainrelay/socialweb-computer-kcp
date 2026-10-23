package kcpclient

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
)

const (
	RootWorkspace = "root"

	ChildLabel = "clusters"
)

const (
	fieldManagerClusterProvider = "socialweb.computer/clusterprovider"

	fieldManagerGateway = "socialweb.computer/gateway"

	fieldManagerRevocation = "socialweb.computer/revocation"
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

func IdentityRecordName(did string) string {
	return AccountHash(did)
}

var clusterKeyShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func ClusterKey(home string) (string, error) {
	parts := strings.Split(home, ":")
	if len(parts) != 3 || parts[0] != RootWorkspace || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf(
			"kcpclient: %q is not a home workspace path of the form %s:<account>:<cluster>, so the "+
				"key a cluster is known by inside a provider cannot be derived from it",
			home, RootWorkspace)
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

const TenantNamespace = "users"

const OwnersGroup = "socialweb.computer:cluster-owners"

func ServiceAccountName(did string) string {
	return "did-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte(did)))
}

const GrantOwnerPlaceholder = "__SOCIALWEB_OWNER_SERVICE_ACCOUNT__"

const GrantESOUserPlaceholder = "__SOCIALWEB_ESO_USER__"

var grantUsername = regexp.MustCompile(`^[a-z0-9]([a-z0-9:.-]*[a-z0-9])?$`)

type GrantScope struct {
	OwnerDID string
	ESOUser  string
}

func RenderTenantGrant(doc string, scope GrantScope) (string, error) {
	if scope.OwnerDID == "" {
		return "", errors.New(
			"kcpclient: the tenant grant needs the owning DID: every name it scopes is derived from it, " +
				"and a grant rendered without one would be scoped to a name nothing owns")
	}
	if scope.ESOUser == "" {
		return "", errors.New(
			"kcpclient: the tenant grant needs the username the External Secrets controller " +
				"authenticates as: the policy admits that controller, and only it besides the " +
				"provisioning admin, to write the vault-backed Secrets in the tenant namespace, so a " +
				"grant rendered without it denies every sync, which reads as a broken ExternalSecret " +
				"rather than as a grant that does not name it")
	}
	if !grantUsername.MatchString(scope.ESOUser) {
		return "", fmt.Errorf(
			"kcpclient: the External Secrets username %q is not of the form %s, and the policy "+
				"compares it inside a CEL string literal, where a quote or a backslash would end it",
			scope.ESOUser, grantUsername)
	}
	for _, p := range []struct{ name, placeholder string }{
		{"the owning DID", GrantOwnerPlaceholder},
		{"the External Secrets username", GrantESOUserPlaceholder},
	} {
		if !strings.Contains(doc, p.placeholder) {
			return "", fmt.Errorf(
				"kcpclient: the tenant grant carries no placeholder for %s (%s), so rendering it would "+
					"apply a grant scoped to whatever is written in the file rather than to the value "+
					"this renderer was handed", p.name, p.placeholder)
		}
	}
	rendered := strings.ReplaceAll(doc, GrantOwnerPlaceholder, ServiceAccountName(scope.OwnerDID))
	return strings.ReplaceAll(rendered, GrantESOUserPlaceholder, scope.ESOUser), nil
}

const (
	AccountWorkspaceType kcpv1alpha1.WorkspaceTypeName = "account"
	HomeWorkspaceType    kcpv1alpha1.WorkspaceTypeName = "cluster"
)

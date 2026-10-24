package kcpclient

import (
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strings"

	kcpv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
)

const (
	fieldManagerClusterProvider = "socialweb.computer/clusterprovider"

	fieldManagerGateway = "socialweb.computer/gateway"

	fieldManagerRevocation = "socialweb.computer/revocation"

	fieldManagerIdentityState = "socialweb.computer/atprotoidentity"
)

const ConditionServiceAccountsErased = "ServiceAccountsErased"

func IdentityRecordName(did string) string {
	return clusterhome.AccountHash(did)
}

const TenantNamespace = "users"

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

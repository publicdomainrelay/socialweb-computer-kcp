package vaultlayout

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	CertAuthPath = "cert"

	NodeRolePrefix   = "node-"
	NodePolicyPrefix = "node-secret-"
	NodeSecretPrefix = "secret/data/k3s/node/"

	RoleESO   = "eso-openbao"
	PolicyESO = "eso-openbao-read"

	PathESODemo = "secret/data/k3s/eso-demo"

	PathTokenLookupSelf = "auth/token/lookup-self"
	PathTokenRevokeSelf = "auth/token/revoke-self"
)

var certIdentityPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func NodeSecretPath(node string) string {
	return NodeSecretPrefix + strings.ToLower(node)
}

func NodeCertRoleName(node string) string {
	return NodeRolePrefix + strings.ToLower(node)
}

func NodePolicyName(node string) string {
	return NodePolicyPrefix + strings.ToLower(node)
}

func ValidateCertIdentity(name string) error {
	if len(name) > 63 || !certIdentityPattern.MatchString(name) {
		return fmt.Errorf(
			"vaultlayout: %q is not a lower-case DNS-1123 label, and a certificate identity is "+
				"used at once as a common name, a cert role name and a per-node secret path "+
				"segment. OpenBao lower-cases policy and role names, so a name that is not already "+
				"lower-case binds the certificate to a role the caller did not write, and a name "+
				"holding a slash reads from a different secret path than the policy grants", name)
	}
	return nil
}

func NodeCertPolicyHCL(node string) string {
	return readPolicy(NodeSecretPath(node))
}

func ESOCertPolicyHCL() string {
	return readPolicy(PathTokenLookupSelf, PathESODemo) + updatePolicy(PathTokenRevokeSelf)
}

func updatePolicy(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "path \"%s\" {\n  capabilities = [\"update\"]\n}\n", p)
	}
	return b.String()
}

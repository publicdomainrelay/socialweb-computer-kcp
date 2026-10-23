package gateway

import (
	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	WebAppAccount        = nodeclaim.AgentAccount
	WebAppAccountNS      = nodeclaim.AgentNamespace
	WebAppGrant          = "socialweb-computer-k8s-web-app"
	RequesterGrant       = "socialweb-computer-k8s-requester"
	foreignAccountPrefix = "system:kcp:serviceaccount:"
)

type WebApp struct {
	FrontKubeconfig string
	ClusterCADir    string
	DownloadsDir    string
}

func WebAppAccountRef(workspace string) string {
	return nodeclaim.AgentRef(workspace)
}

func ForeignAccount(cluster, namespace, name string) string {
	return foreignAccountPrefix + cluster + ":" + namespace + ":" + name
}

func WebAppRootRules(gateway string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/"}, Verbs: []string{"access"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"gateways"}, ResourceNames: []string{gateway}, Verbs: []string{"get"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"openbaoservers"}, ResourceNames: []string{vaultlayout.RootServerName}, Verbs: []string{"get"}},
	}
}

func RequesterRootRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/"}, Verbs: []string{"access"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"openbaoservers"}, ResourceNames: []string{vaultlayout.RootServerName}, Verbs: []string{"get"}},
	}
}

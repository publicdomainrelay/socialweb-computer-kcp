package compose

import (
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	infrarecord "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/infra"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const ScopeAll = "*"

func AppRef(kind, name, ws string) commonv1.ObjectRef {
	return Ref(v1alpha1.GroupName, kind, name, ws)
}

func VaultRefIn(name, ws string) commonv1.ObjectRef {
	return AppRef(KindVault, name, ws)
}

func PolicyEngineRefIn(name, ws string) commonv1.ObjectRef {
	return Ref(policyv1alpha1.GroupName, ComponentKindPolicyEngine, name, ws)
}

func RepoRefIn(did, ws string) commonv1.ObjectRef {
	return AppRef(KindAtprotoRepo, atprotorepo.RepoName(did), ws)
}

func PolicyRunLink(from commonv1.ObjectRef, engine commonv1.ObjectRef, workflows ...string) Link {
	var scope []string
	for _, w := range workflows {
		if w = strings.TrimSpace(w); w != "" {
			scope = append(scope, w)
		}
	}
	scope = append(scope, "at://*")
	return RefLink(from, engine, Cap(policy.CapabilityRun, scope...))
}

func VaultLink(from commonv1.ObjectRef, vaultName string, write bool, prefixes ...string) Link {
	capability := vault.CapabilityRead
	if write {
		capability = vault.CapabilityWrite
	}
	return RefLink(from, VaultRefIn(vaultName, from.Workspace), Cap(capability, cleanPrefixes(prefixes)...))
}

func VaultPKILink(from commonv1.ObjectRef, vaultName string, roles ...string) Link {
	return RefLink(from, VaultRefIn(vaultName, from.Workspace), Cap(vault.CapabilityPKIIssue, cleanPrefixes(roles)...))
}

func cleanPrefixes(prefixes []string) []string {
	var out []string
	for _, p := range prefixes {
		if p = strings.Trim(strings.TrimSpace(p), "/"); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{ScopeAll}
	}
	return out
}

func RepoWriteLink(from commonv1.ObjectRef, repo commonv1.ObjectRef, collections ...string) Link {
	return RefLink(from, repo, Cap(atprotorepo.CapabilityWrite, cleanPrefixes(collections)...))
}

func RepoBackendLink(repo, backend commonv1.ObjectRef) Link {
	return RefLink(repo, backend, Cap(atprotorepo.CapabilityRead), Cap(atprotorepo.CapabilityWrite, ScopeAll), Cap(atprotorepo.CapabilitySubscribe, ScopeAll))
}

func LocalRepoLink(local commonv1.ObjectRef, pdsRef commonv1.ObjectRef, did string) Link {
	return RefLink(local, pdsRef, Cap(pds.CapabilityAccount, did), Cap(pds.CapabilitySync))
}

func RemoteRepoLinks(remote commonv1.ObjectRef, remotePDS commonv1.ObjectRef, identity commonv1.ObjectRef, did string) []Link {
	return []Link{
		RefLink(remote, remotePDS, Cap(pds.CapabilityAccount, did), Cap(pds.CapabilitySync)),
		RefLink(remote, identity, Cap(pds.CapabilitySession, did)),
	}
}

func AccountStatusLink(account commonv1.ObjectRef, pdsRef commonv1.ObjectRef, did string) Link {
	return RefLink(account, pdsRef, Cap(pds.CapabilityAccount, did))
}

func SessionStatusLink(session commonv1.ObjectRef, pdsRef commonv1.ObjectRef, did string) Link {
	return RefLink(session, pdsRef, Cap(pds.CapabilitySession, did))
}

func ServiceAuthLink(from commonv1.ObjectRef, pdsRef commonv1.ObjectRef, did string) Link {
	return RefLink(from, pdsRef, Cap(pds.CapabilityServiceAuth, did))
}

func RootAccountLink(root commonv1.ObjectRef, pdsRef commonv1.ObjectRef) Link {
	return RefLink(root, pdsRef, Cap(pds.CapabilityAccount, ScopeAll), Cap(pds.CapabilitySession, ScopeAll))
}

func PdsEventSourceLink(source commonv1.ObjectRef, repo commonv1.ObjectRef, collections ...string) Link {
	return RefLink(source, repo, Cap(atprotorepo.CapabilitySubscribe, cleanPrefixes(collections)...))
}

func RelayEventSourceLink(source commonv1.ObjectRef, relay commonv1.ObjectRef) Link {
	return RefLink(source, relay, Cap(bus.CapabilityXRPC))
}

func RecordWriteLink(ws, owner string, repo commonv1.ObjectRef, collections ...string) Link {
	sel := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "AtprotoRecord", Workspace: ws,
		MatchLabels: map[string]string{OwnerLabel: owner}}
	return SelectorLink(LinkName("records", owner, repo.Name), sel, repo, Cap(atprotorepo.CapabilityWrite, cleanPrefixes(collections)...))
}

var InfraKinds = []string{"Infra", "EphemeralInfra", "InfraCollection", "EphemeralInfraCollection"}

func InfraLinks(ws string, repo commonv1.ObjectRef) []Link {
	collections := []string{infrarecord.InfraNSID, infrarecord.EphemeralInfraNSID}
	var out []Link
	for _, kind := range InfraKinds {
		sel := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: kind, Workspace: ws}
		out = append(out, SelectorLink(LinkName("infra-read", kind, repo.Name), sel, repo, Cap(atprotorepo.CapabilityRead, collections...)))
	}
	sel := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "AtprotoRecord", Workspace: ws,
		MatchExpressions: []exposurev1alpha1.ComponentLinkSelectorRequirement{{Key: v1alpha1.InfraKindLabel, Operator: exposurev1alpha1.ComponentLinkSelectorExists}}}
	return append(out, SelectorLink(LinkName("infra-records", repo.Name), sel, repo, Cap(atprotorepo.CapabilityWrite, collections...)))
}

func ComputeVMVaultLink(ws, vaultName string, prefixes ...string) Link {
	sel := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: KindComputeVM, Workspace: ws}
	return SelectorLink(LinkName("computevms", vaultName), sel, VaultRefIn(vaultName, ws), Cap(vault.CapabilityRead, cleanPrefixes(prefixes)...))
}

func NodeClaimVaultLink(ws, vaultName string, prefixes ...string) Link {
	sel := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "NodeClaim", Workspace: bus.ScopeAll}
	return SelectorLink(LinkName("nodeclaims", vaultName), sel, VaultRefIn(vaultName, ws), Cap(vault.CapabilityWrite, cleanPrefixes(prefixes)...))
}

func EventSourceRefIn(kind, name, ws string) commonv1.ObjectRef {
	return Ref(eventsv1alpha1.GroupName, kind, name, ws)
}

type SecretUse struct {
	Ref *commonv1.SecretRef

	Write bool
}

func SecretLinks(from commonv1.ObjectRef, uses ...SecretUse) []Link {
	reads := map[string][]string{}
	writes := map[string][]string{}
	vaults := map[string]commonv1.ObjectRef{}
	var order []string
	for _, use := range uses {
		ref := use.Ref
		if ref == nil || ref.Vault == nil || strings.TrimSpace(ref.Vault.Name) == "" {
			continue
		}
		to := AppRef(KindVault, strings.TrimSpace(ref.Vault.Name), workspaceOr(ref.Vault.Workspace, from.Workspace))
		key := to.String()
		if _, ok := vaults[key]; !ok {
			vaults[key] = to
			order = append(order, key)
		}
		scope := strings.Trim(strings.TrimSpace(ref.Mount), "/") + "/" + strings.Trim(strings.TrimSpace(ref.Path), "/")
		reads[key] = append(reads[key], scope)
		if use.Write {
			writes[key] = append(writes[key], scope)
		}
	}
	out := make([]Link, 0, len(order))
	for _, key := range order {
		caps := []exposurev1alpha1.LinkCapability{Cap(vault.CapabilityRead, cleanPrefixes(reads[key])...)}
		if len(writes[key]) > 0 {
			caps = append(caps, Cap(vault.CapabilityWrite, cleanPrefixes(writes[key])...))
		}
		out = append(out, RefLink(from, vaults[key], caps...))
	}
	return out
}

func RemotePDSRefIn(name, ws string) commonv1.ObjectRef {
	return Ref(pdsv1alpha1.GroupName, KindRemotePDS, name, ws)
}

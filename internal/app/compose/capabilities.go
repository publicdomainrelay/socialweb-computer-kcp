package compose

import (
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
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

func SecretReadLinks(from commonv1.ObjectRef, refs ...*commonv1.SecretRef) []Link {
	scopes := map[string][]string{}
	vaults := map[string]commonv1.ObjectRef{}
	var order []string
	for _, ref := range refs {
		if ref == nil || ref.Vault == nil || strings.TrimSpace(ref.Vault.Name) == "" {
			continue
		}
		to := AppRef(KindVault, strings.TrimSpace(ref.Vault.Name), workspaceOr(ref.Vault.Workspace, from.Workspace))
		key := to.String()
		if _, ok := vaults[key]; !ok {
			vaults[key] = to
			order = append(order, key)
		}
		scopes[key] = append(scopes[key], strings.Trim(strings.TrimSpace(ref.Mount), "/")+"/"+strings.Trim(strings.TrimSpace(ref.Path), "/"))
	}
	out := make([]Link, 0, len(order))
	for _, key := range order {
		out = append(out, RefLink(from, vaults[key], Cap(vault.CapabilityRead, cleanPrefixes(scopes[key])...)))
	}
	return out
}

func RemotePDSAuthLinks(from commonv1.ObjectRef, auth pdsv1alpha1.RemotePDSAuth) []Link {
	return SecretReadLinks(from, auth.AccessTokenRef, auth.RefreshTokenRef, auth.DPoPKeyRef)
}

func RemotePDSRefIn(name, ws string) commonv1.ObjectRef {
	return Ref(pdsv1alpha1.GroupName, KindRemotePDS, name, ws)
}

package oauth

import (
	"net/url"
	"slices"
	"strings"
)

const (
	CollectionRBACDID               = "com.publicdomainrelay.temp.auth.allowlist.rbacDid"
	CollectionOffering              = "com.publicdomainrelay.temp.market.offering"
	CollectionBid                   = "com.publicdomainrelay.temp.market.bid"
	CollectionBidsFree              = "com.publicdomainrelay.temp.market.bids.free"
	CollectionBidsX402              = "com.publicdomainrelay.temp.market.bids.x402"
	CollectionReceipt               = "com.publicdomainrelay.temp.market.receipt"
	CollectionReceiptsFree          = "com.publicdomainrelay.temp.market.receipts.free"
	CollectionReceiptsX402          = "com.publicdomainrelay.temp.market.receipts.x402"
	CollectionEvent                 = "com.publicdomainrelay.temp.market.event"
	CollectionConfigWIFSimple       = "com.publicdomainrelay.temp.compute.config.wif.simple"
	CollectionComputeVM             = "com.publicdomainrelay.temp.compute.vm"
	CollectionRFP                   = "com.publicdomainrelay.temp.market.rfp"
	CollectionAccept                = "com.publicdomainrelay.temp.market.accept"
	CollectionVMDelete              = "com.publicdomainrelay.temp.compute.events.vm.delete"
	CollectionVMOnNetwork           = "com.publicdomainrelay.temp.compute.events.vm.onNetwork"
	CollectionVMTerminated          = "com.publicdomainrelay.temp.compute.events.vm.terminated"
	CollectionVMHostKeys            = "com.publicdomainrelay.temp.compute.events.vm.hostKeys"
	CollectionGuestPublicKey        = "com.publicdomainrelay.temp.compute.events.guest.publicKey"
	CollectionGuestSignalWebRTC     = "com.publicdomainrelay.temp.compute.events.guest.signalWebRTC"
	CollectionGuestSecrets          = "com.publicdomainrelay.temp.compute.events.guest.secrets"
	CollectionGuestSecretsApplied   = "com.publicdomainrelay.temp.compute.events.guest.secretsApplied"
	CollectionGuestSecretsRequested = "com.publicdomainrelay.temp.compute.events.guest.secretsRequested"
	CollectionGuestAgentStart       = "com.publicdomainrelay.temp.compute.events.guest.agentStart"
	CollectionGuestRequesterDID     = "com.publicdomainrelay.temp.compute.events.guest.requesterDid"
	CollectionGuestSecretsUpdated   = "com.publicdomainrelay.temp.compute.events.guest.secretsUpdated"
	CollectionGuestR2Start          = "com.publicdomainrelay.temp.compute.events.guest.r2Start"
	CollectionGuestR2Release        = "com.publicdomainrelay.temp.compute.events.guest.r2Release"
	CollectionGuestR2Released       = "com.publicdomainrelay.temp.compute.events.guest.r2Released"
	CollectionGuestMemoryPressure   = "com.publicdomainrelay.temp.compute.events.guest.memoryPressure"
	CollectionGuestOOMKill          = "com.publicdomainrelay.temp.compute.events.guest.oomKill"
	CollectionBadgeBlueKeys         = "com.publicdomainrelay.temp.badgeBlueKeys"
	CollectionHeartbeat             = "com.publicdomainrelay.temp.market.heartbeat"
	CollectionFedproxyRBAC          = "com.fedproxy.rbac"
	CollectionFedproxySSHKey        = "com.fedproxy.sshPublicKey"
	CollectionIngressListen         = "computer.socialweb.ingress.listen"
	CollectionIngressAnswer         = "computer.socialweb.ingress.answer"
	CollectionIngressOffer          = "computer.socialweb.ingress.offer"
	CollectionPeer                  = "computer.socialweb.peer"
	CollectionAssociation           = "com.publicdomainrelay.temp.market.association"
	CollectionAssociationRequest    = "com.publicdomainrelay.temp.market.associationRequest"
	CollectionEndorsement           = "link.ident.endorsement"
	CollectionPolicyGhaLite         = "computer.socialweb.temp.policy.ghalite"
	CollectionPolicyTypeScript      = "computer.socialweb.temp.policy.typescript"
	CollectionInfra                 = "computer.socialweb.infra"
	CollectionEphemeralInfra        = "computer.socialweb.ephemeralInfra"
	CollectionIngressWebRTC         = "com.publicdomainrelay.temp.ingress.webRTC"
	CollectionTangledVouch          = "sh.tangled.graph.vouch"

	RepoIngressWebRTCCreate = "repo:" + CollectionIngressWebRTC + "?action=create"
	RepoTangledVouchWrite   = "repo:" + CollectionTangledVouch + "?action=create&action=update"
	RepoIngressListenWrite  = "repo:" + CollectionIngressListen + "?action=create&action=update"
	RepoIngressAnswerWrite  = "repo:" + CollectionIngressAnswer + "?action=create&action=delete"
	RepoIngressOfferWrite   = "repo:" + CollectionIngressOffer + "?action=create&action=delete"
	RepoPeerWrite           = "repo:" + CollectionPeer + "?action=create&action=update&action=delete"
	RepoAssociationWrite    = "repo:" + CollectionAssociation + "?action=create&action=update&action=delete"
	RepoEndorsementWrite    = "repo:" + CollectionEndorsement + "?action=create&action=delete"

	RPCSubmitRFP         = "rpc:com.publicdomainrelay.temp.market.submitRfp?aud=*"
	RPCSubmitAccept      = "rpc:com.publicdomainrelay.temp.market.submitAccept?aud=*"
	RPCSubmitBid         = "rpc:com.publicdomainrelay.temp.market.submitBid?aud=*"
	RPCSubmitEvent       = "rpc:com.publicdomainrelay.temp.market.submitEvent?aud=*"
	RPCClusterCreate     = "rpc:com.publicdomainrelay.temp.cluster.createCluster?aud=*"
	RPCClusterList       = "rpc:com.publicdomainrelay.temp.cluster.listClusters?aud=*"
	RPCClusterKubeconfig = "rpc:com.publicdomainrelay.temp.cluster.getKubeconfig?aud=*"
	RPCClusterUp         = "rpc:com.publicdomainrelay.temp.cluster.controlPlaneUp?aud=*"
	RPCClusterDown       = "rpc:com.publicdomainrelay.temp.cluster.controlPlaneDown?aud=*"
	RPCClusterDelete     = "rpc:com.publicdomainrelay.temp.cluster.deleteCluster?aud=*"
	RPCAtircMint         = "rpc:chat.atirc.mint?aud=did:web:irc.fedfork.com%23atirc"
	RPCAtircRevoke       = "rpc:chat.atirc.revoke?aud=did:web:irc.fedfork.com%23atirc"
	atircAudPublic       = "did:web:irc.fedfork.com#atirc"
)

var (
	RequesterOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		[]string{repoScope([]string{"create", "update"},
			CollectionComputeVM, CollectionRFP, CollectionAccept, CollectionEvent, CollectionVMDelete,
			CollectionVMOnNetwork, CollectionGuestPublicKey, CollectionGuestSignalWebRTC, CollectionGuestSecrets,
			CollectionGuestAgentStart, CollectionGuestSecretsUpdated, CollectionGuestR2Start, CollectionGuestR2Release,
			CollectionPolicyGhaLite, CollectionPolicyTypeScript)},
		[]string{repoScope([]string{"create", "update", "delete"}, CollectionFedproxyRBAC, CollectionBadgeBlueKeys,
			CollectionAssociation, CollectionAssociationRequest, CollectionInfra, CollectionEphemeralInfra)},
		[]string{RPCSubmitRFP, RPCSubmitAccept, RPCSubmitBid, RPCSubmitEvent},
		[]string{RPCAtircMint, RPCAtircRevoke},
		[]string{RepoTangledVouchWrite},
		RequesterOptionalScopes,
	)

	BidderOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create"},
			CollectionRBACDID, CollectionBid, CollectionBidsFree, CollectionReceipt, CollectionEvent,
			CollectionConfigWIFSimple, CollectionComputeVM, CollectionRFP, CollectionAccept,
			CollectionVMDelete, CollectionVMOnNetwork, CollectionVMTerminated, CollectionVMHostKeys,
			CollectionGuestPublicKey, CollectionGuestSignalWebRTC, CollectionGuestSecretsApplied,
			CollectionGuestSecretsRequested, CollectionGuestRequesterDID, CollectionGuestR2Released,
			CollectionGuestMemoryPressure, CollectionGuestOOMKill,
			CollectionFedproxyRBAC, CollectionPolicyGhaLite, CollectionPolicyTypeScript),
		[]string{collectionScope(CollectionOffering, "create", "update")},
		[]string{RepoIngressListenWrite, RepoIngressAnswerWrite},
		[]string{collectionScope(CollectionHeartbeat, "create", "update")},
		[]string{collectionScope(CollectionBadgeBlueKeys, "create", "update", "delete")},
		[]string{collectionScope(CollectionAssociation, "create", "update", "delete")},
		[]string{RPCSubmitRFP, RPCSubmitAccept, RPCSubmitBid, RPCSubmitEvent},
	)

	DesktopOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create", "update"},
			CollectionRBACDID, CollectionOffering, CollectionBid, CollectionBidsFree, CollectionBidsX402,
			CollectionReceipt, CollectionReceiptsFree, CollectionReceiptsX402, CollectionEvent,
			CollectionConfigWIFSimple, CollectionFedproxyRBAC),
		[]string{collectionScope(CollectionBadgeBlueKeys, "create")},
		[]string{RPCSubmitBid, RPCSubmitEvent},
	)

	ComputeSPAOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create"},
			CollectionComputeVM, CollectionBadgeBlueKeys, CollectionFedproxyRBAC, CollectionFedproxySSHKey),
		[]string{RPCSubmitRFP, RPCSubmitAccept},
	)

	QROAuthScope = dedupeScopes(
		BidderOAuthScope,
		RequesterOAuthScope,
	)

	K8sOAuthScope = dedupeScopes(
		RequesterOAuthScope,
		[]string{RPCClusterCreate, RPCClusterList, RPCClusterKubeconfig, RPCClusterUp, RPCClusterDown, RPCClusterDelete},
	)

	OAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		RequesterOAuthScope,
		BidderOAuthScope,
		DesktopOAuthScope,
		ComputeSPAOAuthScope,
		QROAuthScope,
		K8sOAuthScope,
	)
)

var RequesterOptionalScopes = []string{repoScope([]string{"delete"}, CollectionComputeVM, CollectionRFP, CollectionAccept, CollectionEvent,
	CollectionVMDelete, CollectionVMOnNetwork, CollectionGuestPublicKey, CollectionGuestSignalWebRTC, CollectionGuestSecrets,
	CollectionGuestAgentStart, CollectionGuestSecretsUpdated, CollectionGuestR2Start, CollectionGuestR2Release, CollectionPolicyGhaLite,
	CollectionPolicyTypeScript), RepoIngressOfferWrite, RepoPeerWrite, RepoEndorsementWrite}

var OptionalScopes = slices.Concat([]string{RPCAtircRevoke, RepoTangledVouchWrite, RepoIngressListenWrite, RepoIngressAnswerWrite}, RequesterOptionalScopes)

func RequiredScopes(requested []string) []string {
	out := make([]string, 0, len(requested))
	for _, scope := range requested {
		if !slices.Contains(OptionalScopes, scope) {
			out = append(out, scope)
		}
	}
	return out
}

func ScopeString(scopes []string) string {
	return strings.Join(scopes, " ")
}

const ScopeTransitionGeneric = "transition:generic"

var repoActions = []string{"create", "update", "delete"}

func MissingScopes(required, granted []string) []string {
	have := map[string]bool{}
	for _, scope := range granted {
		have[scope] = true
	}
	missing := []string{}
	for _, want := range required {
		if have[want] || grantedBy(want, granted, have) {
			continue
		}
		missing = append(missing, want)
	}
	return missing
}

var transitionScopes = []string{ScopeTransitionGeneric, "transition:email", "transition:chat.bsky"}

var permissionResources = []string{"repo", "rpc", "blob", "identity", "account", "include"}

func ValidScope(scope string) bool {
	if scope == "" {
		return false
	}
	for _, c := range scope {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	if scope == ScopeAtproto || slices.Contains(transitionScopes, scope) {
		return true
	}
	head, query, _ := strings.Cut(scope, "?")
	resource, _, _ := strings.Cut(head, ":")
	if !slices.Contains(permissionResources, resource) {
		return false
	}
	_, err := url.ParseQuery(query)
	return err == nil
}

func KnownGrant(granted []string) bool {
	return len(granted) > 0 && !slices.ContainsFunc(granted, func(scope string) bool { return !ValidScope(scope) })
}

func ScopeSubject(scope string) string {
	resource, subject, _ := parsePermission(scope)
	if resource == "repo" {
		subject = strings.Join(repoCollections(scope), ",")
	}
	if subject == "" || (resource != "repo" && resource != "rpc") {
		return scope
	}
	return subject
}

func grantedBy(want string, granted []string, have map[string]bool) bool {
	resource, subject, params := parsePermission(want)
	if resource != "repo" && resource != "rpc" {
		return false
	}
	if have[ScopeTransitionGeneric] {
		return true
	}
	if resource == "rpc" {
		for _, scope := range granted {
			gotResource, gotSubject, got := parsePermission(scope)
			if gotResource == resource && (gotSubject == subject || gotSubject == "*") {
				if aud := got.Get("aud"); aud == "*" || aud == params.Get("aud") {
					return true
				}
			}
		}
		return false
	}
	collections := repoCollections(want)
	if len(collections) == 0 {
		return false
	}
	for _, collection := range collections {
		needed := map[string]bool{}
		for _, action := range orAll(params["action"]) {
			needed[action] = true
		}
		for _, scope := range granted {
			gotResource, _, got := parsePermission(scope)
			if gotResource != "repo" {
				continue
			}
			if gotCollections := repoCollections(scope); !slices.Contains(gotCollections, collection) && !slices.Contains(gotCollections, "*") {
				continue
			}
			for _, action := range orAll(got["action"]) {
				delete(needed, action)
			}
		}
		if len(needed) > 0 {
			return false
		}
	}
	return true
}

func repoCollections(scope string) []string {
	head, query, _ := strings.Cut(strings.TrimSpace(scope), "?")
	if _, positional, ok := strings.Cut(head, ":"); ok && positional != "" {
		return []string{positional}
	}
	params, err := url.ParseQuery(query)
	if err != nil {
		return nil
	}
	return params["collection"]
}

func parsePermission(scope string) (string, string, url.Values) {
	head, query, _ := strings.Cut(strings.TrimSpace(scope), "?")
	resource, subject, _ := strings.Cut(head, ":")
	params, err := url.ParseQuery(query)
	if err != nil {
		params = url.Values{}
	}
	if subject == "" {
		switch resource {
		case "repo":
			subject = params.Get("collection")
		case "rpc":
			subject = params.Get("lxm")
		}
	}
	return resource, subject, params
}

func orAll(actions []string) []string {
	if len(actions) == 0 {
		return repoActions
	}
	return actions
}

func collectionScope(nsid string, verbs ...string) string {
	return "repo:" + nsid + "?action=" + strings.Join(verbs, "&action=")
}

func repoScope(verbs []string, nsids ...string) string {
	if len(nsids) == 1 {
		return collectionScope(nsids[0], verbs...)
	}
	query := url.Values{"collection": nsids, "action": verbs}
	return "repo?" + query.Encode()
}

func collectionScopes(verbs []string, nsids ...string) []string {
	out := make([]string, 0, len(nsids))
	for _, nsid := range nsids {
		out = append(out, collectionScope(nsid, verbs...))
	}
	return out
}

func dedupeScopes(sets ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, set := range sets {
		for _, token := range set {
			if seen[token] {
				continue
			}
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

// RPCAtircRevokeFor is RPCAtircRevoke for the atirc service audience aud (a network's atircUrl).
func RPCAtircRevokeFor(aud string) string {
	if aud == "" || aud == atircAudPublic {
		return RPCAtircRevoke
	}
	return "rpc:chat.atirc.revoke?aud=" + strings.ReplaceAll(aud, "#", "%23")
}

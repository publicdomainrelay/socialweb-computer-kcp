package oauth

import (
	"net/url"
	"strings"
)

const (
	CollectionRBACDID           = "com.publicdomainrelay.temp.auth.allowlist.rbacDid"
	CollectionOffering          = "com.publicdomainrelay.temp.market.offering"
	CollectionBid               = "com.publicdomainrelay.temp.market.bid"
	CollectionBidsFree          = "com.publicdomainrelay.temp.market.bids.free"
	CollectionBidsX402          = "com.publicdomainrelay.temp.market.bids.x402"
	CollectionReceipt           = "com.publicdomainrelay.temp.market.receipt"
	CollectionReceiptsFree      = "com.publicdomainrelay.temp.market.receipts.free"
	CollectionReceiptsX402      = "com.publicdomainrelay.temp.market.receipts.x402"
	CollectionEvent             = "com.publicdomainrelay.temp.market.event"
	CollectionConfigWIFSimple   = "com.publicdomainrelay.temp.compute.config.wif.simple"
	CollectionComputeVM         = "com.publicdomainrelay.temp.compute.vm"
	CollectionRFP               = "com.publicdomainrelay.temp.market.rfp"
	CollectionAccept            = "com.publicdomainrelay.temp.market.accept"
	CollectionVMDelete          = "com.publicdomainrelay.temp.compute.events.vm.delete"
	CollectionVMOnNetwork       = "com.publicdomainrelay.temp.compute.events.vm.onNetwork"
	CollectionVMTerminated      = "com.publicdomainrelay.temp.compute.events.vm.terminated"
	CollectionVMHostKeys        = "com.publicdomainrelay.temp.compute.events.vm.hostKeys"
	CollectionGuestPublicKey    = "com.publicdomainrelay.temp.compute.events.guest.publicKey"
	CollectionGuestSignalWebRTC = "com.publicdomainrelay.temp.compute.events.guest.signalWebRTC"
	CollectionBadgeBlueKeys     = "com.publicdomainrelay.temp.badgeBlueKeys"
	CollectionHeartbeat         = "com.publicdomainrelay.temp.market.heartbeat"
	CollectionFedproxyRBAC      = "com.fedproxy.rbac"
	CollectionFedproxySSHKey    = "com.fedproxy.sshPublicKey"
	CollectionBidderAssociation = "com.publicdomainrelay.temp.market.bidderAssociation"
	CollectionPolicyGhaLite     = "computer.socialweb.temp.policy.ghalite"
	CollectionPolicyTypeScript  = "computer.socialweb.temp.policy.typescript"
	CollectionInfra             = "computer.socialweb.infra"
	CollectionEphemeralInfra    = "computer.socialweb.ephemeralInfra"

	RPCSubmitRFP         = "rpc:com.publicdomainrelay.temp.market.submitRfp?aud=*"
	RPCSubmitAccept      = "rpc:com.publicdomainrelay.temp.market.submitAccept?aud=*"
	RPCSubmitBid         = "rpc:com.publicdomainrelay.temp.market.submitBid?aud=*"
	RPCSubmitEvent       = "rpc:com.publicdomainrelay.temp.market.submitEvent?aud=*"
	RPCAssociateConfirm  = "rpc:com.publicdomainrelay.temp.requester.associateConfirm?aud=*"
	RPCClusterCreate     = "rpc:com.publicdomainrelay.temp.cluster.createCluster?aud=*"
	RPCClusterList       = "rpc:com.publicdomainrelay.temp.cluster.listClusters?aud=*"
	RPCClusterKubeconfig = "rpc:com.publicdomainrelay.temp.cluster.getKubeconfig?aud=*"
	RPCClusterUp         = "rpc:com.publicdomainrelay.temp.cluster.controlPlaneUp?aud=*"
	RPCClusterDown       = "rpc:com.publicdomainrelay.temp.cluster.controlPlaneDown?aud=*"
	RPCClusterDelete     = "rpc:com.publicdomainrelay.temp.cluster.deleteCluster?aud=*"
)

var (
	RequesterOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create", "update"},
			CollectionComputeVM, CollectionRFP, CollectionAccept, CollectionEvent, CollectionVMDelete,
			CollectionVMOnNetwork, CollectionGuestPublicKey, CollectionGuestSignalWebRTC,
			CollectionPolicyGhaLite, CollectionPolicyTypeScript),
		[]string{collectionScope(CollectionFedproxyRBAC, "create", "update", "delete")},
		[]string{collectionScope(CollectionBadgeBlueKeys, "create", "update", "delete")},
		collectionScopes([]string{"create", "update", "delete"}, CollectionInfra, CollectionEphemeralInfra),
		[]string{RPCSubmitRFP, RPCSubmitAccept, RPCSubmitBid, RPCSubmitEvent},
	)

	BidderOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create"},
			CollectionRBACDID, CollectionBid, CollectionBidsFree, CollectionReceipt, CollectionEvent,
			CollectionConfigWIFSimple, CollectionComputeVM, CollectionRFP, CollectionAccept,
			CollectionVMDelete, CollectionVMOnNetwork, CollectionVMTerminated, CollectionVMHostKeys,
			CollectionGuestPublicKey, CollectionGuestSignalWebRTC,
			CollectionFedproxyRBAC, CollectionBidderAssociation, CollectionPolicyGhaLite,
			CollectionPolicyTypeScript),
		[]string{collectionScope(CollectionOffering, "create", "update")},
		[]string{collectionScope(CollectionHeartbeat, "create", "update")},
		[]string{collectionScope(CollectionBadgeBlueKeys, "create", "update", "delete")},
		[]string{RPCSubmitRFP, RPCSubmitAccept, RPCSubmitBid, RPCSubmitEvent},
	)

	DesktopOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create", "update"},
			CollectionRBACDID, CollectionOffering, CollectionBid, CollectionBidsFree, CollectionBidsX402,
			CollectionReceipt, CollectionReceiptsFree, CollectionReceiptsX402, CollectionEvent,
			CollectionConfigWIFSimple, CollectionFedproxyRBAC, CollectionBidderAssociation),
		[]string{collectionScope(CollectionBadgeBlueKeys, "create")},
		[]string{RPCSubmitBid, RPCSubmitEvent},
	)

	ComputeSPAOAuthScope = dedupeScopes(
		[]string{ScopeAtproto},
		collectionScopes([]string{"create"},
			CollectionComputeVM, CollectionBadgeBlueKeys, CollectionFedproxyRBAC, CollectionFedproxySSHKey),
		[]string{RPCSubmitRFP, RPCSubmitAccept},
	)

	DIDKeyAssociatorOAuthScope = dedupeScopes(
		BidderOAuthScope,
		RequesterOAuthScope,
		[]string{RPCAssociateConfirm},
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
		DIDKeyAssociatorOAuthScope,
		K8sOAuthScope,
	)
)

const OAuthScopeLocalhost = "atproto repo:com.publicdomainrelay.temp.badgeBlueKeys?action=create,update,delete rpc:com.publicdomainrelay.temp.requester.associateConfirm?aud=*"

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

func ScopeSubject(scope string) string {
	resource, subject, _ := parsePermission(scope)
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
	needed := map[string]bool{}
	if resource == "repo" {
		for _, action := range orAll(params["action"]) {
			needed[action] = true
		}
	}
	for _, scope := range granted {
		gotResource, gotSubject, got := parsePermission(scope)
		if gotResource != resource || (gotSubject != subject && gotSubject != "*") {
			continue
		}
		if resource == "rpc" {
			if aud := got.Get("aud"); aud == "*" || aud == params.Get("aud") {
				return true
			}
			continue
		}
		for _, action := range orAll(got["action"]) {
			delete(needed, action)
		}
		if len(needed) == 0 {
			return true
		}
	}
	return false
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

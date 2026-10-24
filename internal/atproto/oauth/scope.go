package oauth

import "strings"

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

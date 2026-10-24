package v1alpha1

import (
	"net"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type MarketNetworkValue struct {
	PLCDirectoryURL string `json:"plcDirectoryUrl" apigen:"userEndpoint"`

	IngressProxyHost string `json:"ingressProxyHost"`

	QrOrigin string `json:"qrOrigin"`

	CABundle string `json:"caBundle,omitempty"`

	Hosts []NetworkHost `json:"hosts,omitempty" apigen:"listType=map,listMapKey=name"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`
}

type NetworkHost struct {
	Name string `json:"name"`

	Address string `json:"address"`
}

const (
	NetworkDispatcherHost = "dispatcher-tls.default.relay.svc.kcp.local"

	NetworkRelayHost = "relay.default.relay.svc.kcp.local"

	GuestTLSHost = "relay.localhost"
)

func (n *MarketNetworkValue) HostAddress(name string) string {
	if n == nil {
		return ""
	}
	for _, h := range n.Hosts {
		if h.Name == name {
			return strings.TrimSpace(h.Address)
		}
	}
	return ""
}

func (n *MarketNetworkValue) RelayURL() string {
	if address := n.HostAddress(NetworkRelayHost); address != "" {
		return "http://" + address
	}
	return ""
}

func (n *MarketNetworkValue) Relays() []string {
	if u := n.RelayURL(); u != "" {
		return []string{u}
	}
	return nil
}

func (n *MarketNetworkValue) GuestTLSURL() string {
	_, port, err := net.SplitHostPort(n.HostAddress(NetworkDispatcherHost))
	if err != nil || port == "" || port == "0" {
		return ""
	}
	return "https://" + GuestTLSHost + ":" + port
}

type ApplicationKind string

const (
	ApplicationKindMarketBidder ApplicationKind = "MarketBidder"

	ApplicationKindMarketRequester ApplicationKind = "MarketRequester"

	ApplicationKindGateway ApplicationKind = "Gateway"

	ApplicationKindSshGateway ApplicationKind = "SshGateway"
)

type ApplicationPhase string

const (
	ApplicationPhasePending ApplicationPhase = "Pending"

	ApplicationPhaseProvisioning ApplicationPhase = "Provisioning"

	ApplicationPhaseReady ApplicationPhase = "Ready"

	ApplicationPhaseDeleting ApplicationPhase = "Deleting"

	ApplicationPhaseFailed ApplicationPhase = "Failed"

	ApplicationPhaseSuspended ApplicationPhase = "Suspended"
)

type ApplicationOption struct {
	Name string `json:"name"`

	Value string `json:"value,omitempty"`
}

type ApplicationIdentity struct {
	Mode MarketIdentityMode `json:"mode,omitempty"`

	Handle string `json:"handle,omitempty"`

	Did string `json:"did,omitempty"`

	PdsURL string `json:"pdsUrl,omitempty" apigen:"userEndpoint"`

	PdsRef *commonv1.ObjectRef `json:"pdsRef,omitempty"`

	Association AssociationMode `json:"association,omitempty"`

	ApprovedAssociations []string `json:"approvedAssociations,omitempty" apigen:"listType=set"`
}

type MarketCommsMode string

const (
	MarketCommsModeXRPC MarketCommsMode = "XRPC"

	MarketCommsModeFirehose MarketCommsMode = "Firehose"

	MarketCommsModeBoth MarketCommsMode = "Both"
)

type MarketComms struct {
	Mode MarketCommsMode `json:"mode,omitempty"`

	EventSourceRefs []commonv1.ObjectRef `json:"eventSourceRefs,omitempty"`
}

type IngressConfig struct {
	Provider string `json:"provider,omitempty"`

	IngressRef *commonv1.ObjectRef `json:"ingressRef,omitempty"`

	PublicHost string `json:"publicHost,omitempty"`

	Xrpc *IngressXrpcConfig `json:"xrpc,omitempty"`

	OpenTunnel *IngressProxyOpenTunnel `json:"opentunnel,omitempty"`
}

type IngressXrpcConfig struct {
	IngressProxyHost string `json:"ingressProxyHost,omitempty"`

	RelayURL string `json:"relayURL,omitempty" apigen:"userEndpoint"`

	RelayRef *commonv1.ObjectRef `json:"relayRef,omitempty"`

	IdentityRef *commonv1.ObjectRef `json:"identityRef,omitempty"`
}

type FirecrackerProvider struct {
	WorkRoot string `json:"workRoot"`

	VmruntimeCommand []string `json:"vmruntimeCommand"`
}

type KcpProvider struct {
	Handle string `json:"handle,omitempty"`

	VmHostBackend string `json:"vmHostBackend,omitempty"`

	GuestImageSource string `json:"guestImageSource"`

	StopGraceSeconds int32 `json:"stopGraceSeconds,omitempty"`
}

type ApplicationListener struct {
	Component string `json:"component"`

	Name string `json:"name"`

	Kind string `json:"kind"`

	Address string `json:"address"`
}

type ApplicationComponent struct {
	Kind string `json:"kind"`

	Name string `json:"name"`

	Workspace string `json:"workspace"`

	Phase string `json:"phase,omitempty"`

	Ready bool `json:"ready"`

	Suspended bool `json:"suspended,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Restarts int32 `json:"restarts,omitempty"`

	Message string `json:"message,omitempty"`
}

type ComponentSummary struct {
	Desired int `json:"desired"`

	Ready int `json:"ready"`

	Suspended int `json:"suspended"`

	NotReady int `json:"notReady"`
}

type ComponentOverrideStatus struct {
	Component string `json:"component"`

	Generation int64 `json:"generation,omitempty"`

	Suspend bool `json:"suspend,omitempty"`

	RestartPolicy string `json:"restartPolicy,omitempty"`

	Applied bool `json:"applied"`

	Message string `json:"message,omitempty"`
}

type AppliedSet struct {
	Generation int64 `json:"generation,omitempty"`

	Digest string `json:"digest,omitempty"`
}

type ApplicationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	AppliedSet *AppliedSet `json:"appliedSet,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Profile string `json:"profile,omitempty"`

	Did string `json:"did,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	ComponentSummary *ComponentSummary `json:"componentSummary,omitempty"`

	ComponentOverrides []ComponentOverrideStatus `json:"componentOverrides,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`

	StateDir *StateDirUsage `json:"stateDir,omitempty"`

	Store *StoreUsage `json:"store,omitempty"`

	GC *GCStatus `json:"gc,omitempty"`

	TTL *TTLStatus `json:"ttl,omitempty"`

	Quotas []QuotaUsage `json:"quotas,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Market *MarketLoopStatus `json:"market,omitempty"`

	BidderStreams []BidderStreamStatus `json:"bidderStreams,omitempty"`
}

type BidderStreamStatus struct {
	EventStream string `json:"eventStream"`

	Pds string `json:"pds"`

	Dids []string `json:"dids,omitempty"`

	Active bool `json:"active,omitempty"`

	SeenAt string `json:"seenAt,omitempty"`
}

const AnnotationTTLAfterFinished = "socialweb.computer/ttl-after-finished"

type TTLDeleted struct {
	Kind string `json:"kind"`

	Count int64 `json:"count"`

	LastAt string `json:"lastAt,omitempty"`
}

type TTLStatus struct {
	SampledAt string `json:"sampledAt"`

	Tracked int64 `json:"tracked"`

	DeletedTotal int64 `json:"deletedTotal"`

	Deleted []TTLDeleted `json:"deleted,omitempty"`
}

type ApplicationQuota struct {
	Bids int32 `json:"bids,omitempty"`

	ComputeVMs int32 `json:"computeVMs,omitempty"`

	ComputeContracts int32 `json:"computeContracts,omitempty"`

	NodeClaims int32 `json:"nodeClaims,omitempty"`

	PolicyWorkflowRuns int32 `json:"policyWorkflowRuns,omitempty"`
}

type QuotaUsage struct {
	Workspace string `json:"workspace"`

	Resource string `json:"resource"`

	Hard int64 `json:"hard"`

	Used int64 `json:"used"`
}

type GCKindCount struct {
	Kind string `json:"kind"`

	Count int64 `json:"count"`

	Examples []string `json:"examples,omitempty"`
}

type GCStuck struct {
	Kind string `json:"kind"`

	Finalizer string `json:"finalizer"`

	Controller string `json:"controller"`

	Count int64 `json:"count"`

	Examples []string `json:"examples,omitempty"`
}

type GCStatus struct {
	SampledAt string `json:"sampledAt"`

	Objects int64 `json:"objects"`

	OrphanTotal int64 `json:"orphanTotal"`

	Orphans []GCKindCount `json:"orphans,omitempty"`

	StuckTotal int64 `json:"stuckTotal"`

	Terminating []GCStuck `json:"terminating,omitempty"`
}

type MarketLoopStatus struct {
	RfpsSeen int64 `json:"rfpsSeen,omitempty"`

	RfpsScoped int64 `json:"rfpsScoped,omitempty"`

	DeclinedByReason []MarketDeclineCount `json:"declinedByReason,omitempty"`

	Bids int64 `json:"bids,omitempty"`

	Wins int64 `json:"wins,omitempty"`

	ContractsActive int64 `json:"contractsActive,omitempty"`

	Streams []MarketStreamStatus `json:"streams,omitempty"`

	RecentDeclines []MarketDecline `json:"recentDeclines,omitempty"`

	RfpToBid *MarketLatencyStatus `json:"rfpToBid,omitempty"`

	KcpWait *MarketLatencyStatus `json:"kcpWait,omitempty"`

	PdsWrites *PdsWriteStatus `json:"pdsWrites,omitempty"`

	UpdatedAt string `json:"updatedAt,omitempty"`
}

type PdsWriteStatus struct {
	Since string `json:"since"`

	Writes int64 `json:"writes"`

	Points int64 `json:"points"`

	RateLimited int64 `json:"rateLimited,omitempty"`

	Reserve int64 `json:"reserve"`

	ByCollection []PdsWriteCollection `json:"byCollection,omitempty"`

	PerHour []PdsWriteHour `json:"perHour,omitempty"`

	Limits []PdsLimitStatus `json:"limits,omitempty"`
}

type PdsWriteCollection struct {
	Collection string `json:"collection"`

	Writes int64 `json:"writes"`

	Points int64 `json:"points"`
}

type PdsWriteHour struct {
	Hour string `json:"hour"`

	Points int64 `json:"points"`
}

type PdsLimitStatus struct {
	Pds string `json:"pds"`

	Limit int64 `json:"limit,omitempty"`

	Remaining *int64 `json:"remaining,omitempty"`

	ResetAt string `json:"resetAt,omitempty"`

	PausedUntil string `json:"pausedUntil,omitempty"`

	ObservedAt string `json:"observedAt"`
}

type MarketLatencyStatus struct {
	Count int64 `json:"count"`

	P50Us int64 `json:"p50Us"`

	P95Us int64 `json:"p95Us"`

	MaxUs int64 `json:"maxUs"`
}

type MarketDecline struct {
	RfpURI string `json:"rfpUri"`

	Reason string `json:"reason"`

	At string `json:"at"`
}

type MarketDeclineCount struct {
	Reason string `json:"reason"`

	Count int64 `json:"count"`
}

type MarketStreamStatus struct {
	Source string `json:"source"`

	Cursor int64 `json:"cursor,omitempty"`

	LagMs int64 `json:"lagMs,omitempty"`

	LastEventAt string `json:"lastEventAt,omitempty"`
}

type ExecutionStatus struct {
	Step string `json:"step,omitempty"`

	StepStartedAt string `json:"stepStartedAt,omitempty"`

	LastEventAt string `json:"lastEventAt,omitempty"`

	LastError string `json:"lastError,omitempty"`

	Attempts int32 `json:"attempts,omitempty"`
}

type StateDirUsage struct {
	Path string `json:"path"`

	Bytes int64 `json:"bytes"`

	Files int64 `json:"files"`

	SampledAt string `json:"sampledAt"`
}

type StoreUsage struct {
	Kind string `json:"kind"`

	Path string `json:"path,omitempty"`

	Bytes int64 `json:"bytes"`

	FreeBytes int64 `json:"freeBytes,omitempty"`

	Rows int64 `json:"rows,omitempty"`

	DeadTuples int64 `json:"deadTuples,omitempty"`

	AutoVacuum string `json:"autoVacuum,omitempty"`

	Vacuums int64 `json:"vacuums,omitempty"`

	ReclaimedBytes int64 `json:"reclaimedBytes,omitempty"`

	LastVacuumAt string `json:"lastVacuumAt,omitempty"`

	SampledAt string `json:"sampledAt"`
}

type MarketBidder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarketBidderSpec  `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

type MarketBidderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MarketBidder `json:"items"`
}

type MarketBidderSpec struct {
	Profile string `json:"profile"`

	Suspend bool `json:"suspend,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	Policy string `json:"policy,omitempty"`

	GuestNetwork *GuestNetworkOptions `json:"guestNetwork,omitempty"`

	Firecracker *FirecrackerProvider `json:"firecracker,omitempty"`

	Kcp *KcpProvider `json:"kcp,omitempty"`

	SigningKeyRotation int32 `json:"signingKeyRotation,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	Components ComponentOverrides `json:"components,omitempty"`

	Operator string `json:"operator,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty" apigen:"listType=set"`
}

type MarketRequester struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarketRequesterSpec `json:"spec,omitempty"`
	Status ApplicationStatus   `json:"status,omitempty"`
}

type MarketRequesterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MarketRequester `json:"items"`
}

type MarketRequesterSpec struct {
	Profile string `json:"profile"`

	Suspend bool `json:"suspend,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity"`

	Comms *MarketComms `json:"comms,omitempty"`

	SecretsFallback SecretsFallback `json:"secretsFallback,omitempty" apigen:"enum=None|Sealed"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	Components ComponentOverrides `json:"components,omitempty"`
}

type Gateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewaySpec       `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

type GatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Gateway `json:"items"`
}

type GatewaySpec struct {
	Profile string `json:"profile"`

	Suspend bool `json:"suspend,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	Components ComponentOverrides `json:"components,omitempty"`

	MarketNetwork *GatewayMarketNetwork `json:"marketNetwork,omitempty"`

	WebAppFront string `json:"webAppFront,omitempty"`

	ClusterProvider *GatewayClusterProvider `json:"clusterProvider,omitempty"`

	Operator string `json:"operator,omitempty"`
}

type GatewayClusterProvider struct {
	ScaleHold string `json:"scaleHold,omitempty"`

	ScaleMaxNodes int32 `json:"scaleMaxNodes,omitempty"`

	ScalePodsPerNode int32 `json:"scalePodsPerNode,omitempty"`
}

type GatewayMarketNetwork struct {
	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty" apigen:"userEndpoint"`

	NodeGuestHostAliases []string `json:"nodeGuestHostAliases,omitempty"`

	NodeUserDataBaseFile string `json:"nodeUserDataBaseFile,omitempty" apigen:"userEndpoint"`
}

type SshGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SshGatewaySpec    `json:"spec,omitempty"`
	Status ApplicationStatus `json:"status,omitempty"`
}

type SshGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SshGateway `json:"items"`
}

type SshGatewaySpec struct {
	Profile string `json:"profile"`

	Suspend bool `json:"suspend,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty" apigen:"userEndpoint"`

	Components ComponentOverrides `json:"components,omitempty"`
}

type MarketNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarketNetworkSpec   `json:"spec,omitempty"`
	Status MarketNetworkStatus `json:"status,omitempty"`
}

type MarketNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MarketNetwork `json:"items"`
}

type MarketNetworkSpec struct {
	Profile string `json:"profile"`

	Suspend bool `json:"suspend,omitempty"`

	GuestLabel string `json:"guestLabel,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	Components ComponentOverrides `json:"components,omitempty"`

	Apps []NetworkApp `json:"apps,omitempty" apigen:"listType=map,listMapKey=name"`
}

type NetworkApp struct {
	Name string `json:"name"`

	Kind ApplicationKind `json:"kind"`

	Profile string `json:"profile"`

	Policy string `json:"policy,omitempty"`

	GuestNetwork *GuestNetworkOptions `json:"guestNetwork,omitempty"`

	Firecracker *FirecrackerProvider `json:"firecracker,omitempty"`

	Kcp *KcpProvider `json:"kcp,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty" apigen:"userEndpoint"`

	ClusterProvider *GatewayClusterProvider `json:"clusterProvider,omitempty"`

	SecretsFallback SecretsFallback `json:"secretsFallback,omitempty" apigen:"enum=None|Sealed"`

	Components ComponentOverrides `json:"components,omitempty"`

	Suspend bool `json:"suspend,omitempty"`

	Stopped bool `json:"stopped,omitempty"`
}

type NetworkAppStatus struct {
	Name string `json:"name"`

	Kind ApplicationKind `json:"kind"`

	Instance string `json:"instance,omitempty"`

	KcpURL string `json:"kcpUrl,omitempty"`

	Workspace string `json:"workspace,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Did string `json:"did,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`
}

type MarketNetworkStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	AppliedSet *AppliedSet `json:"appliedSet,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	BidderPdsURL string `json:"bidderPdsUrl,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	ComponentSummary *ComponentSummary `json:"componentSummary,omitempty"`

	ComponentOverrides []ComponentOverrideStatus `json:"componentOverrides,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`

	Apps []NetworkAppStatus `json:"apps,omitempty"`

	StateDir *StateDirUsage `json:"stateDir,omitempty"`

	GC *GCStatus `json:"gc,omitempty"`

	TTL *TTLStatus `json:"ttl,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

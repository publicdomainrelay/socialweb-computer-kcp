package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MarketNetworkValue struct {
	PlcDirectoryUrl string `json:"plcDirectoryUrl"`

	IngressProxyHost string `json:"ingressProxyHost"`

	RelayUrls []string `json:"relayUrls"`

	FirehoseUrls []string `json:"firehoseUrls"`

	QrOrigin string `json:"qrOrigin"`

	GuestTlsUrl string `json:"guestTlsUrl,omitempty"`

	CaBundleRef string `json:"caBundleRef,omitempty"`

	Hosts []NetworkHost `json:"hosts,omitempty"`
}

type NetworkHost struct {
	Name string `json:"name"`

	Address string `json:"address"`
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
)

type ApplicationOption struct {
	Name string `json:"name"`

	Value string `json:"value,omitempty"`
}

type ApplicationIdentity struct {
	Mode MarketIdentityMode `json:"mode"`

	Handle string `json:"handle,omitempty"`

	PdsUrl string `json:"pdsUrl,omitempty"`

	Association AssociationMode `json:"association,omitempty"`
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
}

type ApplicationStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Profile string `json:"profile,omitempty"`

	Did string `json:"did,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`

	StateDir *StateDirUsage `json:"stateDir,omitempty"`

	Store *StoreUsage `json:"store,omitempty"`

	GC *GCStatus `json:"gc,omitempty"`

	TTL *TTLStatus `json:"ttl,omitempty"`

	Quotas []QuotaUsage `json:"quotas,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Market *MarketLoopStatus `json:"market,omitempty"`
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

	DenoRuns int32 `json:"denoRuns,omitempty"`
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
	RfpUri string `json:"rfpUri"`

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

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity"`

	Policy string `json:"policy,omitempty"`

	Firecracker *FirecrackerProvider `json:"firecracker,omitempty"`

	Kcp *KcpProvider `json:"kcp,omitempty"`

	SigningKeyRotation int32 `json:"signingKeyRotation,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	Operator string `json:"operator,omitempty"`
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

	Network *MarketNetworkValue `json:"network,omitempty"`

	Identity ApplicationIdentity `json:"identity"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`
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

	Network *MarketNetworkValue `json:"network,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

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
	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`

	NodeGuestHostAliases []string `json:"nodeGuestHostAliases,omitempty"`

	NodeUserDataBaseFile string `json:"nodeUserDataBaseFile,omitempty"`
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

	Network *MarketNetworkValue `json:"network,omitempty"`

	BidderDids []string `json:"bidderDids,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`
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

	GuestLabel string `json:"guestLabel,omitempty"`

	Apps []NetworkApp `json:"apps,omitempty"`
}

type NetworkApp struct {
	Name string `json:"name"`

	Kind ApplicationKind `json:"kind"`

	Profile string `json:"profile"`

	Policy string `json:"policy,omitempty"`

	Firecracker *FirecrackerProvider `json:"firecracker,omitempty"`

	Kcp *KcpProvider `json:"kcp,omitempty"`

	Options []ApplicationOption `json:"options,omitempty"`

	Quota *ApplicationQuota `json:"quota,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`

	ClusterProvider *GatewayClusterProvider `json:"clusterProvider,omitempty"`

	Stopped bool `json:"stopped,omitempty"`
}

type NetworkAppStatus struct {
	Name string `json:"name"`

	Kind ApplicationKind `json:"kind"`

	Instance string `json:"instance,omitempty"`

	KcpUrl string `json:"kcpUrl,omitempty"`

	KcpRoot string `json:"kcpRoot,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Did string `json:"did,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`
}

type MarketNetworkStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ApplicationPhase `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	BidderPdsUrl string `json:"bidderPdsUrl,omitempty"`

	Components []ApplicationComponent `json:"components,omitempty"`

	Listeners []ApplicationListener `json:"listeners,omitempty"`

	Apps []NetworkAppStatus `json:"apps,omitempty"`

	StateDir *StateDirUsage `json:"stateDir,omitempty"`

	GC *GCStatus `json:"gc,omitempty"`

	TTL *TTLStatus `json:"ttl,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

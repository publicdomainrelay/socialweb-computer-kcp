package v1alpha1

import (
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ComponentRestartPolicy string

const (
	ComponentRestartNever ComponentRestartPolicy = "Never"

	ComponentRestartOnFailure ComponentRestartPolicy = "OnFailure"

	ComponentRestartAlways ComponentRestartPolicy = "Always"
)

type ComponentPhase string

const (
	ComponentPhasePending ComponentPhase = "Pending"

	ComponentPhaseStarting ComponentPhase = "Starting"

	ComponentPhaseRunning ComponentPhase = "Running"

	ComponentPhaseReady ComponentPhase = "Ready"

	ComponentPhaseFailed ComponentPhase = "Failed"

	ComponentPhaseStopping ComponentPhase = "Stopping"

	ComponentPhaseSuspended ComponentPhase = "Suspended"
)

type ComponentRef struct {
	Kind string `json:"kind,omitempty"`

	Name string `json:"name,omitempty"`

	Did string `json:"did,omitempty"`
}

func (r *ComponentRef) VaultServer(fallback string) string {
	if r == nil || strings.TrimSpace(r.Name) == "" {
		return fallback
	}
	if strings.EqualFold(strings.TrimSpace(r.Kind), "RemoteOpenBao") {
		return "RemoteOpenBao/" + strings.TrimSpace(r.Name)
	}
	return strings.TrimSpace(r.Name)
}

type ComponentIdentity struct {
	MarketIdentityRef *ComponentRef `json:"marketIdentityRef,omitempty"`

	Did string `json:"did,omitempty"`

	Handle string `json:"handle,omitempty"`
}

type ComponentLimits struct {
	CpuMillicores int32 `json:"cpuMillicores,omitempty"`

	MemoryBytes int64 `json:"memoryBytes,omitempty"`

	DiskBytes int64 `json:"diskBytes,omitempty"`
}

type ComponentTLS struct {
	Enabled bool `json:"enabled,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	CertRef string `json:"certRef,omitempty"`

	CaBundleRef string `json:"caBundleRef,omitempty"`
}

type ComponentServiceAuth struct {
	Issuer string `json:"issuer,omitempty"`

	Audience []string `json:"audience,omitempty"`

	Hostname string `json:"hostname,omitempty"`

	KeyRef string `json:"keyRef,omitempty"`
}

type ComponentOverrides map[string]ComponentOverride

type ComponentOverride struct {
	Suspend *bool `json:"suspend,omitempty"`

	RestartPolicy ComponentRestartPolicy `json:"restartPolicy,omitempty"`

	Limits *ComponentLimits `json:"limits,omitempty"`

	ServiceAuth *ComponentServiceAuthOverride `json:"serviceAuth,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`

	Ingress *IngressConfig `json:"ingress,omitempty"`

	Settings *ComponentSettings `json:"settings,omitempty"`
}

type ComponentServiceAuthOverride struct {
	Audience []string `json:"audience,omitempty"`

	Hostname string `json:"hostname,omitempty"`
}

type ComponentSettings struct {
	Bidder *BidderSettings `json:"bidder,omitempty"`

	Requester *RequesterSettings `json:"requester,omitempty"`

	GuestControlPlane *GuestControlPlaneSettings `json:"guestControlPlane,omitempty"`

	Pds *PdsSettings `json:"pds,omitempty"`

	SshServer *SshServerSettings `json:"sshServer,omitempty"`

	GatewayAPI *GatewayAPISettings `json:"gatewayAPI,omitempty"`

	IngressProxy *IngressProxySettings `json:"ingressProxy,omitempty"`

	Relay *RelaySettings `json:"relay,omitempty"`

	PolicyEngine *PolicyEngineSettings `json:"policyEngine,omitempty"`
}

type BidderSettings struct {
	HeartbeatIntervalSeconds *int32 `json:"heartbeatIntervalSeconds,omitempty"`

	OfferingRefreshSeconds *int32 `json:"offeringRefreshSeconds,omitempty"`

	BidTTLSeconds *int32 `json:"bidTtlSeconds,omitempty"`

	MaxConcurrentGuests *int32 `json:"maxConcurrentGuests,omitempty"`

	PolicyPollMilliseconds *int32 `json:"policyPollMilliseconds,omitempty"`

	PolicyTimeoutSeconds *int32 `json:"policyTimeoutSeconds,omitempty"`
}

type RequesterSettings struct {
	MaxConcurrent *int32 `json:"maxConcurrent,omitempty"`

	PerAccount *int32 `json:"perAccount,omitempty"`

	ContractPollMilliseconds *int32 `json:"contractPollMilliseconds,omitempty"`

	BidWindowSeconds *int32 `json:"bidWindowSeconds,omitempty"`

	ReceiptTimeoutSeconds *int32 `json:"receiptTimeoutSeconds,omitempty"`

	ReceiptPollMilliseconds *int32 `json:"receiptPollMilliseconds,omitempty"`

	SessionCapSeconds *int32 `json:"sessionCapSeconds,omitempty"`

	SessionIdleSeconds *int32 `json:"sessionIdleSeconds,omitempty"`

	DirectoryTTLSeconds *int32 `json:"directoryTTLSeconds,omitempty"`

	DirectoryKeys *int32 `json:"directoryKeys,omitempty"`

	PolicyPollMilliseconds *int32 `json:"policyPollMilliseconds,omitempty"`

	PolicyTimeoutSeconds *int32 `json:"policyTimeoutSeconds,omitempty"`

	OAuthScopes []string `json:"oauthScopes,omitempty"`
}

type GuestControlPlaneSettings struct {
	GuestLoginMount string `json:"guestLoginMount,omitempty"`

	Role string `json:"role,omitempty"`

	GrantSyncSeconds *int32 `json:"grantSyncSeconds,omitempty"`

	IssuerRetrySeconds *int32 `json:"issuerRetrySeconds,omitempty"`

	HostKeyAttempts *int32 `json:"hostKeyAttempts,omitempty"`

	RelayEnabled *bool `json:"relayEnabled,omitempty"`

	HostKeyRef string `json:"hostKeyRef,omitempty"`
}

type PdsSettings struct {
	MaxBlobSize *int64 `json:"maxBlobSize,omitempty"`

	MaxRequestBytes *int64 `json:"maxRequestBytes,omitempty"`

	ContactEmail string `json:"contactEmail,omitempty"`

	OAuth *PdsOAuthSettings `json:"oauth,omitempty"`
}

type PdsOAuthSettings struct {
	AllowLocalhostClients *bool `json:"allowLocalhostClients,omitempty"`

	AccessTokenTTLSeconds *int32 `json:"accessTokenTTLSeconds,omitempty"`

	RefreshTokenTTLSeconds *int32 `json:"refreshTokenTTLSeconds,omitempty"`

	DisableLocalhostClients *bool `json:"disableLocalhostClients,omitempty"`

	Scopes []string `json:"scopes,omitempty"`
}

type SshServerSettings struct {
	MaxConnections *int32 `json:"maxConnections,omitempty"`

	MaxSessions *int32 `json:"maxSessions,omitempty"`

	SessionsPerAccount *int32 `json:"sessionsPerAccount,omitempty"`

	MaxAuthAttempts *int32 `json:"maxAuthAttempts,omitempty"`

	AuthTimeoutSeconds *int32 `json:"authTimeoutSeconds,omitempty"`

	SessionMaxSeconds *int32 `json:"sessionMaxSeconds,omitempty"`

	CacheEntries *int32 `json:"cacheEntries,omitempty"`
}

type GatewayAPISettings struct {
	NodePollIntervalSeconds *int32 `json:"nodePollIntervalSeconds,omitempty"`

	RequiredDownloads []string `json:"requiredDownloads,omitempty"`
}

type IngressProxySettings struct {
	RelayTimeoutMilliseconds *int32 `json:"relayTimeoutMilliseconds,omitempty"`

	KeepaliveMilliseconds *int32 `json:"keepaliveMilliseconds,omitempty"`

	KeepaliveTimeoutMilliseconds *int32 `json:"keepaliveTimeoutMilliseconds,omitempty"`

	ReconnectGraceMilliseconds *int32 `json:"reconnectGraceMilliseconds,omitempty"`

	NonceTTLMilliseconds *int32 `json:"nonceTTLMilliseconds,omitempty"`
}

type RelaySettings struct {
	CrawlIntervalSeconds *int32 `json:"crawlIntervalSeconds,omitempty"`
}

type PolicyEngineSettings struct {
	Limits *PolicyEngineLimitsSettings `json:"limits,omitempty"`

	Trust *PolicyEngineTrustSettings `json:"trust,omitempty"`

	Cache *PolicyEngineCacheSettings `json:"cache,omitempty"`
}

type PolicyEngineLimitsSettings struct {
	MaxConcurrentRuns *int32 `json:"maxConcurrentRuns,omitempty"`

	QueueDepth *int32 `json:"queueDepth,omitempty"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	MaxConsoleBytes *int64 `json:"maxConsoleBytes,omitempty"`

	MaxOutputBytes *int64 `json:"maxOutputBytes,omitempty"`

	MaxCacheEntries *int32 `json:"maxCacheEntries,omitempty"`

	MaxCacheBytes *int64 `json:"maxCacheBytes,omitempty"`
}

type PolicyEngineTrustSettings struct {
	TTLSeconds *int64 `json:"ttlSeconds,omitempty"`
}

type PolicyEngineCacheSettings struct {
	VerdictTTLSeconds *int64 `json:"verdictTTLSeconds,omitempty"`

	ScopeTTLSeconds *int64 `json:"scopeTTLSeconds,omitempty"`

	RecordTTLSeconds *int64 `json:"recordTTLSeconds,omitempty"`
}

type ComponentStorage struct {
	DataDir string `json:"dataDir,omitempty"`

	BlobsDir string `json:"blobsDir,omitempty"`

	CertsDir string `json:"certsDir,omitempty"`
}

type ComponentServe struct {
	Host string `json:"host,omitempty"`

	Port int32 `json:"port,omitempty"`

	HealthPath string `json:"healthPath,omitempty"`
}

type ComponentSpec struct {
	Suspend bool `json:"suspend,omitempty"`

	RestartPolicy ComponentRestartPolicy `json:"restartPolicy,omitempty"`

	StateDir string `json:"stateDir,omitempty"`

	Network *MarketNetworkValue `json:"network,omitempty"`

	Limits *ComponentLimits `json:"limits,omitempty"`

	TLS *ComponentTLS `json:"tls,omitempty"`

	ServiceAuth *ComponentServiceAuth `json:"serviceAuth,omitempty"`
}

type Endpoint struct {
	Name string `json:"name"`

	Url string `json:"url,omitempty"`

	Address string `json:"address,omitempty"`

	Protocol string `json:"protocol,omitempty"`
}

type ComponentStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ComponentPhase `json:"phase,omitempty"`

	Ready bool `json:"ready"`

	Endpoint string `json:"endpoint,omitempty"`

	Endpoints []Endpoint `json:"endpoints,omitempty"`

	Message string `json:"message,omitempty"`

	Restarts int32 `json:"restarts,omitempty"`

	StartedAt string `json:"startedAt,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type Bidder struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BidderSpec      `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type BidderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Bidder `json:"items"`
}

type BidderSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	PdsRef *ComponentRef `json:"pdsRef,omitempty"`

	PDSAccessTokenPath string `json:"pdsAccessTokenPath,omitempty"`

	PolicyEngineRef *ComponentRef `json:"policyEngineRef,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	GuestImageSource string `json:"guestImageSource,omitempty"`

	Firecracker *FirecrackerProvider `json:"firecracker,omitempty"`

	Kcp *KcpProvider `json:"kcp,omitempty"`

	MaxConcurrentGuests int32 `json:"maxConcurrentGuests,omitempty"`

	HeartbeatIntervalSeconds *int32 `json:"heartbeatIntervalSeconds,omitempty"`

	OfferingRefreshSeconds *int32 `json:"offeringRefreshSeconds,omitempty"`

	BidTTLSeconds *int32 `json:"bidTtlSeconds,omitempty"`

	PolicyPollMilliseconds *int32 `json:"policyPollMilliseconds,omitempty"`

	PolicyTimeoutSeconds *int32 `json:"policyTimeoutSeconds,omitempty"`

	Operator string `json:"operator,omitempty"`

	Policy string `json:"policy,omitempty"`

	PolicyArgs string `json:"policyArgs,omitempty"`

	BidderDIDs []string `json:"bidderDIDs,omitempty"`

	MarketURL string `json:"marketURL,omitempty"`

	Mode string `json:"mode,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`
}

type GuestControlPlane struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GuestControlPlaneSpec `json:"spec,omitempty"`
	Status ComponentStatus       `json:"status,omitempty"`
}

type GuestControlPlaneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GuestControlPlane `json:"items"`
}

type GuestControlPlaneSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	PdsRef *ComponentRef `json:"pdsRef,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	HostKeyRef string `json:"hostKeyRef,omitempty"`

	GuestLoginMount string `json:"guestLoginMount,omitempty"`

	Role string `json:"role,omitempty"`

	GrantSyncSeconds *int32 `json:"grantSyncSeconds,omitempty"`

	IssuerRetrySeconds *int32 `json:"issuerRetrySeconds,omitempty"`

	HostKeyAttempts *int32 `json:"hostKeyAttempts,omitempty"`

	RelayEnabled *bool `json:"relayEnabled,omitempty"`
}

type PdsMode string

const (
	PdsModeRepo PdsMode = "Repo"

	PdsModeFull PdsMode = "Full"
)

type Pds struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PdsSpec         `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type PdsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Pds `json:"items"`
}

type PdsSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	Mode PdsMode `json:"mode,omitempty"`

	PlcURL string `json:"plcURL,omitempty"`

	PublicBaseURL string `json:"publicBaseURL,omitempty"`

	RequireIngress bool `json:"requireIngress,omitempty"`

	IngressRef *ComponentRef `json:"ingressRef,omitempty"`

	PublicURL string `json:"publicURL,omitempty"`

	StableHost string `json:"stableHost,omitempty"`

	RepoKeyPath string `json:"repoKeyPath,omitempty"`

	SessionSecretPath string `json:"sessionSecretPath,omitempty"`

	RotationKeyPath string `json:"rotationKeyPath,omitempty"`

	AllowExplicitDID bool `json:"allowExplicitDID,omitempty"`

	MaxBlobSize *int64 `json:"maxBlobSize,omitempty"`

	MaxRequestBytes *int64 `json:"maxRequestBytes,omitempty"`

	ContactEmail string `json:"contactEmail,omitempty"`

	OAuth *PdsOAuth `json:"oauth,omitempty"`
}

type PdsOAuth struct {
	Issuer string `json:"issuer,omitempty"`

	AllowLocalhostClients bool `json:"allowLocalhostClients,omitempty"`

	AccessTokenTTLSeconds int32 `json:"accessTokenTTLSeconds,omitempty"`

	RefreshTokenTTLSeconds int32 `json:"refreshTokenTTLSeconds,omitempty"`

	DisableLocalhostClients bool `json:"disableLocalhostClients,omitempty"`

	Scopes []string `json:"scopes,omitempty"`
}

type Requester struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RequesterSpec   `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type RequesterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Requester `json:"items"`
}

type RequesterSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	PdsRef *ComponentRef `json:"pdsRef,omitempty"`

	PDSAccessTokenPath string `json:"pdsAccessTokenPath,omitempty"`

	PolicyEngineRef *ComponentRef `json:"policyEngineRef,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	PublicOrigin string `json:"publicOrigin,omitempty"`

	OAuthClientID string `json:"oauthClientID,omitempty"`

	OAuthRedirectURI string `json:"oauthRedirectURI,omitempty"`

	Mode string `json:"mode,omitempty"`

	GuestSSHKeysFile string `json:"guestSSHKeysFile,omitempty"`

	BidderDIDs []string `json:"bidderDIDs,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`

	IrohSocket string `json:"irohSocket,omitempty"`

	AcceptCopies int32 `json:"acceptCopies,omitempty"`

	MaxConcurrent int32 `json:"maxConcurrent,omitempty"`

	PerAccount int32 `json:"perAccount,omitempty"`

	ContractPollMilliseconds *int32 `json:"contractPollMilliseconds,omitempty"`

	BidWindowSeconds *int32 `json:"bidWindowSeconds,omitempty"`

	ReceiptTimeoutSeconds *int32 `json:"receiptTimeoutSeconds,omitempty"`

	ReceiptPollMilliseconds *int32 `json:"receiptPollMilliseconds,omitempty"`

	SessionCapSeconds *int32 `json:"sessionCapSeconds,omitempty"`

	SessionIdleSeconds *int32 `json:"sessionIdleSeconds,omitempty"`

	DirectoryTTLSeconds *int32 `json:"directoryTTLSeconds,omitempty"`

	DirectoryKeys *int32 `json:"directoryKeys,omitempty"`

	PolicyPollMilliseconds *int32 `json:"policyPollMilliseconds,omitempty"`

	PolicyTimeoutSeconds *int32 `json:"policyTimeoutSeconds,omitempty"`

	OAuthScopes []string `json:"oauthScopes,omitempty"`

	Comms *MarketComms `json:"comms,omitempty"`
}

type GatewayAPI struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayAPISpec  `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type GatewayAPIList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []GatewayAPI `json:"items"`
}

type GatewayAPISpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	PdsRef *ComponentRef `json:"pdsRef,omitempty"`

	RequesterRef *ComponentRef `json:"requesterRef,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	ClusterProvider *GatewayClusterProvider `json:"clusterProvider,omitempty"`

	ClusterStateDir string `json:"clusterStateDir,omitempty"`

	KubeconfigCAFile string `json:"kubeconfigCAFile,omitempty"`

	PublicOrigin string `json:"publicOrigin,omitempty"`

	Audience string `json:"audience,omitempty"`

	RequiredDownloads []string `json:"requiredDownloads,omitempty"`

	PostSignInURL string `json:"postSignInURL,omitempty"`

	NodeMarket *GatewayNodeMarket `json:"nodeMarket,omitempty"`

	NodePollIntervalSeconds *int32 `json:"nodePollIntervalSeconds,omitempty"`
}

type GatewayNodeMarket struct {
	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`

	UserDataBaseFile string `json:"userDataBaseFile,omitempty"`

	HomeWorkspace string `json:"homeWorkspace,omitempty"`

	ContractWorkspace string `json:"contractWorkspace,omitempty"`

	NodePolicy string `json:"nodePolicy,omitempty"`

	NodePolicyArgs string `json:"nodePolicyArgs,omitempty"`

	ExtraBidderDIDs []string `json:"extraBidderDIDs,omitempty"`

	PollIntervalSeconds int32 `json:"pollIntervalSeconds,omitempty"`
}

type SshServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SshServerSpec   `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type SshServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []SshServer `json:"items"`
}

type SshServerSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	RequesterRef *ComponentRef `json:"requesterRef,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	HostKeyRef string `json:"hostKeyRef,omitempty"`

	OwnerSessionsFile string `json:"ownerSessionsFile,omitempty"`

	MaxConnections int32 `json:"maxConnections,omitempty"`

	MaxSessions int32 `json:"maxSessions,omitempty"`

	SessionsPerAccount int32 `json:"sessionsPerAccount,omitempty"`

	MaxAuthAttempts int32 `json:"maxAuthAttempts,omitempty"`

	AuthTimeoutSeconds *int32 `json:"authTimeoutSeconds,omitempty"`

	SessionMaxSeconds *int32 `json:"sessionMaxSeconds,omitempty"`

	CacheEntries int32 `json:"cacheEntries,omitempty"`
}

type Plc struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlcSpec         `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type PlcList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Plc `json:"items"`
}

type PlcSpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`
}

type Relay struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RelaySpec       `json:"spec,omitempty"`
	Status ComponentStatus `json:"status,omitempty"`
}

type RelayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Relay `json:"items"`
}

type RelaySpec struct {
	ComponentSpec `json:",inline"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	PdsRefs []ComponentRef `json:"pdsRefs,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	CrawlIntervalSeconds *int32 `json:"crawlIntervalSeconds,omitempty"`
}

type IngressProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   IngressProxySpec `json:"spec,omitempty"`
	Status ComponentStatus  `json:"status,omitempty"`
}

type IngressProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []IngressProxy `json:"items"`
}

const (
	IngressProviderXRPCSubscription = "xrpc-subscription"

	IngressProviderIroh = "iroh"

	IngressProviderOpenTunnel = "opentunnel"
)

type IngressProxySpec struct {
	ComponentSpec `json:",inline"`

	Provider string `json:"provider,omitempty"`

	Identity *ComponentIdentity `json:"identity,omitempty"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	RelayRef *ComponentRef `json:"relayRef,omitempty"`

	RelayURL string `json:"relayURL,omitempty"`

	IngressProxyHost string `json:"ingressProxyHost,omitempty"`

	IdentityRef *ComponentRef `json:"identityRef,omitempty"`

	PublicHost string `json:"publicHost,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	Iroh *IngressProxyIroh `json:"iroh,omitempty"`

	OpenTunnel *IngressProxyOpenTunnel `json:"opentunnel,omitempty"`

	RelayTimeoutMilliseconds *int32 `json:"relayTimeoutMilliseconds,omitempty"`

	KeepaliveMilliseconds *int32 `json:"keepaliveMilliseconds,omitempty"`

	KeepaliveTimeoutMilliseconds *int32 `json:"keepaliveTimeoutMilliseconds,omitempty"`

	ReconnectGraceMilliseconds *int32 `json:"reconnectGraceMilliseconds,omitempty"`

	NonceTTLMilliseconds *int32 `json:"nonceTTLMilliseconds,omitempty"`
}

type IngressProxyIroh struct {
	DumbpipePath string `json:"dumbpipePath,omitempty"`

	RelayURL string `json:"relayURL,omitempty"`

	ListenHost string `json:"listenHost,omitempty"`
}

type IngressProxyOpenTunnel struct {
	Command []string `json:"command,omitempty"`

	RouteName string `json:"routeName,omitempty"`

	ProxyProtocol string `json:"proxyProtocol,omitempty"`

	LocalHost string `json:"localHost,omitempty"`
}

type DispatcherTLS struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DispatcherTLSSpec `json:"spec,omitempty"`
	Status ComponentStatus   `json:"status,omitempty"`
}

type DispatcherTLSList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []DispatcherTLS `json:"items"`
}

type DispatcherTLSSpec struct {
	ComponentSpec `json:",inline"`

	VaultRef *ComponentRef `json:"vaultRef,omitempty"`

	UpstreamRef *ComponentRef `json:"upstreamRef,omitempty"`

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`
}

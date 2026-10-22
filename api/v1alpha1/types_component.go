package v1alpha1

import (
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

	KeyRef string `json:"keyRef,omitempty"`
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

	Operator string `json:"operator,omitempty"`

	Policy string `json:"policy,omitempty"`

	PolicyArgs string `json:"policyArgs,omitempty"`

	BidderDIDs []string `json:"bidderDIDs,omitempty"`
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

	RepoKeyPath string `json:"repoKeyPath,omitempty"`

	SessionSecretPath string `json:"sessionSecretPath,omitempty"`

	RotationKeyPath string `json:"rotationKeyPath,omitempty"`

	AllowExplicitDID bool `json:"allowExplicitDID,omitempty"`

	OAuth *PdsOAuth `json:"oauth,omitempty"`
}

type PdsOAuth struct {
	Issuer string `json:"issuer,omitempty"`

	AllowLocalhostClients bool `json:"allowLocalhostClients,omitempty"`

	AccessTokenTTLSeconds int32 `json:"accessTokenTTLSeconds,omitempty"`

	RefreshTokenTTLSeconds int32 `json:"refreshTokenTTLSeconds,omitempty"`

	DisableLocalhostClients bool `json:"disableLocalhostClients,omitempty"`
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

	AcceptCopies int32 `json:"acceptCopies,omitempty"`
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

	PublicOrigin string `json:"publicOrigin,omitempty"`

	Audience string `json:"audience,omitempty"`

	RequiredDownloads []string `json:"requiredDownloads,omitempty"`

	PostSignInURL string `json:"postSignInURL,omitempty"`

	NodeMarket *GatewayNodeMarket `json:"nodeMarket,omitempty"`
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

	Hostnames []string `json:"hostnames,omitempty"`

	Storage *ComponentStorage `json:"storage,omitempty"`

	Serve *ComponentServe `json:"serve,omitempty"`

	Iroh *IngressProxyIroh `json:"iroh,omitempty"`

	OpenTunnel *IngressProxyOpenTunnel `json:"opentunnel,omitempty"`
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

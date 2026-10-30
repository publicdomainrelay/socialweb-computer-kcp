package v1alpha1

import (
	"cmp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ComputeVM struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ComputeVMSpec   `json:"spec,omitempty"`
	Status ComputeVMStatus `json:"status,omitempty"`
}

type ComputeVMList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ComputeVM `json:"items"`
}

type ComputeVMSpec struct {
	BackendRef commonv1.ObjectRef `json:"backendRef"`

	VM ComputeVMShape `json:"vm"`

	Owner ComputeVMOwner `json:"owner"`

	UserData ComputeVMUserData `json:"userData"`

	Requester ComputeVMRequester `json:"requester"`

	Contract *ComputeVMContract `json:"contract,omitempty"`

	Image *ComputeVMImageRequest `json:"image,omitempty"`

	StopGraceSeconds int32 `json:"stopGraceSeconds,omitempty"`

	GuestLogin *ComputeVMGuestLogin `json:"guestLogin,omitempty"`

	Accept *commonv1.StrongRef `json:"accept,omitempty"`

	Release *ComputeVMRelease `json:"release,omitempty"`

	HostPorts []int32 `json:"hostPorts,omitempty" apigen:"listType=set"`

	Sandbox *ComputeVMSandbox `json:"sandbox,omitempty"`

	GuestNetwork *GuestNetworkOptions `json:"guestNetwork,omitempty"`
}

type ComputeVMSandbox struct {
	Source string `json:"source"`

	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

type GuestNetworkOptions struct {
	LANUDP *bool `json:"lanUDP,omitempty"`

	HostUDP *bool `json:"hostUDP,omitempty"`

	WebRTCUDPPorts *GuestUDPPortRange `json:"webrtcUDPPorts,omitempty"`

	WebRTCUDPPort int32 `json:"webrtcUDPPort,omitempty" apigen:"minimum=0,maximum=65535"`
}

type GuestUDPPortRange struct {
	Min int32 `json:"min" apigen:"minimum=1024,maximum=65535"`

	Max int32 `json:"max" apigen:"minimum=1024,maximum=65535"`
}

const (
	DefaultWebRTCUDPPortMin int32 = 31000

	DefaultWebRTCUDPPortMax int32 = 31999
)

const (
	BidderOnlyMePolicy       = "bidder-only-me"
	BidderTangledVouchPolicy = "bidder-tangled-vouch"
	BidderDefaultPolicy      = BidderTangledVouchPolicy

	HostUDPReasonSpec      = "spec"
	HostUDPReasonOnlyMe    = "policy only-me"
	HostUDPReasonNotOnlyMe = "policy not only-me"
)

type GuestNetworkStatus struct {
	LANUDP bool `json:"lanUDP"`

	HostUDP bool `json:"hostUDP"`

	HostUDPReason string `json:"hostUDPReason,omitempty"`

	WebRTCUDPPorts *GuestUDPPortRange `json:"webrtcUDPPorts,omitempty"`
}

func (g *GuestNetworkOptions) LANUDPAllowed() bool {
	return g == nil || g.LANUDP == nil || *g.LANUDP
}

func (g *GuestNetworkOptions) HostUDPAllowed() bool {
	return g != nil && g.HostUDP != nil && *g.HostUDP
}

func (g *GuestNetworkOptions) WebRTCUDPPortRange() GuestUDPPortRange {
	if g == nil || g.WebRTCUDPPorts == nil {
		return GuestUDPPortRange{Min: DefaultWebRTCUDPPortMin, Max: DefaultWebRTCUDPPortMax}
	}
	return *g.WebRTCUDPPorts
}

func (g *GuestNetworkOptions) WebRTCUDPPortOf() int32 {
	if g == nil {
		return 0
	}
	return g.WebRTCUDPPort
}

func BidderPolicyOnlyMe(policy string) bool {
	return strings.TrimSpace(policy) == BidderOnlyMePolicy
}

func (g *GuestNetworkOptions) EffectiveFor(policy string) GuestNetworkStatus {
	ports := g.WebRTCUDPPortRange()
	st := GuestNetworkStatus{LANUDP: g.LANUDPAllowed(), WebRTCUDPPorts: &ports}
	switch {
	case g != nil && g.HostUDP != nil:
		st.HostUDP, st.HostUDPReason = *g.HostUDP, HostUDPReasonSpec
	case BidderPolicyOnlyMe(policy):
		st.HostUDP, st.HostUDPReason = true, HostUDPReasonOnlyMe
	default:
		st.HostUDPReason = HostUDPReasonNotOnlyMe
	}
	return st
}

func (g *GuestNetworkOptions) WithDefaults(d *GuestNetworkOptions) *GuestNetworkOptions {
	out := GuestNetworkOptions{}
	if g != nil {
		out = *g
	}
	if d != nil {
		out.LANUDP = cmp.Or(out.LANUDP, d.LANUDP)
		out.HostUDP = cmp.Or(out.HostUDP, d.HostUDP)
		out.WebRTCUDPPorts = cmp.Or(out.WebRTCUDPPorts, d.WebRTCUDPPorts)
	}
	return &out
}

func GuestNetworkWith(lanUDP, hostUDP bool, webrtcUDPPort int32) *GuestNetworkOptions {
	return &GuestNetworkOptions{LANUDP: &lanUDP, HostUDP: &hostUDP, WebRTCUDPPort: webrtcUDPPort}
}

type ComputeVMRelease struct {
	RequestedAt string `json:"requestedAt"`

	Request *commonv1.StrongRef `json:"request,omitempty"`
}

type ComputeVMGuestLogin struct {
	Mount string `json:"mount"`

	BindSecretSha256 string `json:"bindSecretSha256"`
}

type ComputeVMUserData struct {
	Vault commonv1.SecretRef `json:"vault"`
}

type ComputeVMShape struct {
	Cpus int32 `json:"cpus"`

	Mem string `json:"mem"`

	Disk string `json:"disk"`

	Role string `json:"role"`
}

type ComputeVMOwner struct {
	Handle string `json:"handle"`
}

type ComputeVMRequester struct {
	Did string `json:"did"`

	Plc string `json:"plc,omitempty"`
}

type ComputeVMContract struct {
	Receipt commonv1.StrongRef `json:"receipt,omitempty"`

	Accept commonv1.StrongRef `json:"accept,omitempty"`

	SubmitEventURL string `json:"submitEventUrl,omitempty" apigen:"userEndpoint"`

	AcceptedAt string `json:"acceptedAt,omitempty"`
}

type ComputeVMImageRequest struct {
	ReuseStale bool `json:"reuseStale,omitempty"`
}

type ComputeVMPhase string

const (
	ComputeVMPhasePending ComputeVMPhase = "Pending"

	ComputeVMPhasePreparing ComputeVMPhase = "Preparing"

	ComputeVMPhaseBooting ComputeVMPhase = "Booting"

	ComputeVMPhaseRunning ComputeVMPhase = "Running"

	ComputeVMPhaseReleasing ComputeVMPhase = "Releasing"

	ComputeVMPhaseLost ComputeVMPhase = "Lost"

	ComputeVMPhaseFailed ComputeVMPhase = "Failed"
)

type ComputeVMLostReason string

const (
	ComputeVMLostHostRebooted ComputeVMLostReason = "HostRebooted"

	ComputeVMLostUnitGone ComputeVMLostReason = "UnitGone"

	ComputeVMLostUnitFailed ComputeVMLostReason = "UnitFailed"

	ComputeVMLostOOMKilled ComputeVMLostReason = "OOMKilled"

	ComputeVMLostVMMExited ComputeVMLostReason = "VMMExited"

	ComputeVMLostGuestPoweroff ComputeVMLostReason = "GuestPoweroff"

	ComputeVMLostIdentityMismatch ComputeVMLostReason = "IdentityMismatch"
)

type ComputeVMAddresses struct {
	Guest string `json:"guest,omitempty"`

	Host string `json:"host,omitempty"`
}

type ComputeVMHostKey struct {
	Type string `json:"type"`

	PublicKey string `json:"publicKey"`

	Fingerprint string `json:"fingerprint,omitempty"`
}

type ComputeVMMarket struct {
	Guest *ComputeVMGuestReport `json:"guest,omitempty"`

	Published *ComputeVMPublished `json:"published,omitempty"`
}

type ComputeVMGuestReport struct {
	Address string `json:"address,omitempty"`

	ReportedAt string `json:"reportedAt,omitempty"`

	HostKeys []ComputeVMHostKey `json:"hostKeys,omitempty"`

	Networks []ComputeVMGuestNetwork `json:"networks,omitempty"`
}

type ComputeVMGuestNetwork struct {
	Tag string `json:"tag"`

	Kind NetworkKind `json:"kind,omitempty" apigen:"enum=iroh|https|webrtc"`

	Address string `json:"address"`

	ReportedAt string `json:"reportedAt,omitempty"`
}

type ComputeVMPublished struct {
	OnNetworkURI string `json:"onNetworkUri,omitempty"`

	HostKeysURI string `json:"hostKeysUri,omitempty"`

	Networks []ComputeVMPublishedNetwork `json:"networks,omitempty"`
}

type ComputeVMPublishedNetwork struct {
	Tag string `json:"tag"`

	Kind NetworkKind `json:"kind,omitempty" apigen:"enum=iroh|https|webrtc"`

	URI string `json:"uri"`
}

type ComputeVMTermination struct {
	Reason string `json:"reason,omitempty"`

	ObservedAt string `json:"observedAt,omitempty"`

	ExitCode *int `json:"exitCode,omitempty"`

	Request *commonv1.StrongRef `json:"request,omitempty"`

	PayloadURI string `json:"payloadUri,omitempty"`

	PayloadCid string `json:"payloadCid,omitempty"`

	EventURI string `json:"eventUri,omitempty"`

	EventCid string `json:"eventCid,omitempty"`
}

type ComputeVMStatus struct {
	Phase ComputeVMPhase `json:"phase,omitempty"`

	LostReason ComputeVMLostReason `json:"lostReason,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Addresses *ComputeVMAddresses `json:"addresses,omitempty"`

	BootedAt string `json:"bootedAt,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Market *ComputeVMMarket `json:"market,omitempty"`
}

const (
	ComputeVMConditionReady = commonv1.ConditionReady

	ComputeVMConditionBackendBound = commonv1.ConditionBackendBound
)

const BackendKindFirecrackerVM = "FirecrackerVM"

const BackendKindDenoSandbox = "DenoSandbox"

const FinalizerComputeVM = "computevm.socialweb.computer/release"

const (
	FieldManagerComputeVMCreator = "socialweb.computer/bidder"

	FieldManagerComputeVMController = "socialweb.computer/computevm"

	FieldManagerComputeVMMarketGuest = "socialweb.computer/guestcontrolplane"

	FieldManagerComputeVMMarketPublished = "socialweb.computer/bidder"
)

const GuestHostAddress = "192.0.2.1"

const (
	GuestVaultPrefix = "guests"

	GuestUserDataEntry = "user-data"

	GuestUserDataKey = "userData"

	GuestProvisioningEntry = "provisioning"
)

package v1alpha1

import (
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

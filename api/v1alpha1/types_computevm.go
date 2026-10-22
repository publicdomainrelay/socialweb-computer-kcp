package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	VM ComputeVMShape `json:"vm"`

	Owner ComputeVMOwner `json:"owner"`

	UserData ComputeVMUserData `json:"userData"`

	Requester ComputeVMRequester `json:"requester"`

	Contract *ComputeVMContract `json:"contract,omitempty"`

	Image *ComputeVMImageRequest `json:"image,omitempty"`

	StopGraceSeconds int32 `json:"stopGraceSeconds,omitempty"`

	GuestLogin *ComputeVMGuestLogin `json:"guestLogin,omitempty"`
}

type ComputeVMGuestLogin struct {
	Mount string `json:"mount"`

	BindSecretSha256 string `json:"bindSecretSha256"`
}

type ComputeVMUserData struct {
	Vault ComputeVMVaultRef `json:"vault"`
}

type ComputeVMVaultRef struct {
	Server string `json:"server"`

	Mount string `json:"mount"`

	Path string `json:"path"`
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
	ReceiptUri string `json:"receiptUri,omitempty"`

	ReceiptCid string `json:"receiptCid,omitempty"`

	AcceptUri string `json:"acceptUri,omitempty"`

	AcceptCid string `json:"acceptCid,omitempty"`

	SubmitEventUrl string `json:"submitEventUrl,omitempty"`

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

	ComputeVMLostIdentityMismatch ComputeVMLostReason = "IdentityMismatch"
)

type ComputeVMNetwork struct {
	GuestIP string `json:"guestIP,omitempty"`

	HostAddress string `json:"hostAddress,omitempty"`

	Prefix int32 `json:"prefix,omitempty"`

	MAC string `json:"mac,omitempty"`
}

type ComputeVMImage struct {
	Ref string `json:"ref,omitempty"`

	Sha256 string `json:"sha256,omitempty"`

	Hypervisor string `json:"hypervisor,omitempty"`
}

type ComputeVMRuntime struct {
	Backend string `json:"backend,omitempty"`

	Ref string `json:"ref,omitempty"`

	Instance string `json:"instance,omitempty"`

	Pid int64 `json:"pid,omitempty"`

	StartTime string `json:"startTime,omitempty"`

	HostBootID string `json:"hostBootID,omitempty"`

	VMMID string `json:"vmmID,omitempty"`
}

type ComputeVMHostKey struct {
	Type string `json:"type"`

	PublicKey string `json:"publicKey"`

	Fingerprint string `json:"fingerprint,omitempty"`
}

type ComputeVMMarket struct {
	OnNetworkUri string `json:"onNetworkUri,omitempty"`

	Address string `json:"address,omitempty"`

	HostKeys []ComputeVMHostKey `json:"hostKeys,omitempty"`

	HostKeysUri string `json:"hostKeysUri,omitempty"`

	ReportedAt string `json:"reportedAt,omitempty"`
}

type ComputeVMStatus struct {
	Phase ComputeVMPhase `json:"phase,omitempty"`

	LostReason ComputeVMLostReason `json:"lostReason,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Network *ComputeVMNetwork `json:"network,omitempty"`

	Image *ComputeVMImage `json:"image,omitempty"`

	Runtime *ComputeVMRuntime `json:"runtime,omitempty"`

	WorkDir string `json:"workDir,omitempty"`

	BootedAt string `json:"bootedAt,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`

	Market *ComputeVMMarket `json:"market,omitempty"`
}

const (
	ConditionPrepared = "Prepared"

	ConditionStarted = "Started"

	ConditionRuntimeObserved = "RuntimeObserved"

	ConditionGuestReported = "GuestReported"

	ConditionMarketReported = "MarketReported"
)

const FinalizerComputeVM = "computevm.socialweb.computer/release"

const (
	FieldManagerComputeVMController = "computevm-controller"

	FieldManagerComputeVMProvider = "compute-provider-kcp"

	FieldManagerComputeVMContract = "compute-provider-kcp-contract"

	FieldManagerComputeVMGuest = "compute-provider-kcp-guest"
)

const GuestHostAddress = "192.0.2.1"

const (
	GuestVaultPrefix = "guests"

	GuestUserDataEntry = "user-data"

	GuestUserDataKey = "userData"

	GuestProvisioningEntry = "provisioning"
)

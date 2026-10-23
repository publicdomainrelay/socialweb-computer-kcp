package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type FirecrackerVM struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FirecrackerVMSpec   `json:"spec,omitempty"`
	Status FirecrackerVMStatus `json:"status,omitempty"`
}

type FirecrackerVMList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []FirecrackerVM `json:"items"`
}

type FirecrackerVMSpec struct {
	Owner ComputeVMOwner `json:"owner"`

	Vcpu int32 `json:"vcpu"`

	Mem string `json:"mem"`

	Disk string `json:"disk"`

	Image *ComputeVMImageRequest `json:"image,omitempty"`

	UserData ComputeVMUserData `json:"userData"`

	StopGraceSeconds int32 `json:"stopGraceSeconds,omitempty"`
}

type FirecrackerVMNetwork struct {
	GuestIP string `json:"guestIP,omitempty"`

	HostAddress string `json:"hostAddress,omitempty"`

	Prefix int32 `json:"prefix,omitempty"`

	MAC string `json:"mac,omitempty"`

	Tap string `json:"tap,omitempty"`
}

type FirecrackerVMImage struct {
	Ref string `json:"ref,omitempty"`

	Sha256 string `json:"sha256,omitempty"`

	Hypervisor string `json:"hypervisor,omitempty"`
}

type FirecrackerVMRuntime struct {
	Backend string `json:"backend,omitempty"`

	Ref string `json:"ref,omitempty"`

	Instance string `json:"instance,omitempty"`

	Pid int64 `json:"pid,omitempty"`

	StartTime string `json:"startTime,omitempty"`

	HostBootID string `json:"hostBootID,omitempty"`

	VMMID string `json:"vmmID,omitempty"`
}

type FirecrackerVMStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Phase ComputeVMPhase `json:"phase,omitempty"`

	LostReason ComputeVMLostReason `json:"lostReason,omitempty"`

	Network *FirecrackerVMNetwork `json:"network,omitempty"`

	Image *FirecrackerVMImage `json:"image,omitempty"`

	Runtime *FirecrackerVMRuntime `json:"runtime,omitempty"`

	WorkDir string `json:"workDir,omitempty"`

	BootedAt string `json:"bootedAt,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const FieldManagerFirecrackerVM = "socialweb.computer/firecrackervm"

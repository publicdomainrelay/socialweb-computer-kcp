package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type MarketIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MarketIdentitySpec   `json:"spec,omitempty"`
	Status MarketIdentityStatus `json:"status,omitempty"`
}

type MarketIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MarketIdentity `json:"items"`
}

type MarketIdentityMode string

const (
	MarketIdentityModeOAuthQr MarketIdentityMode = "oauthQr"

	MarketIdentityModeOAuthBrowser MarketIdentityMode = "oauthBrowser"

	MarketIdentityModePdsAccount MarketIdentityMode = "pdsAccount"

	MarketIdentityModeLocalRepo MarketIdentityMode = "localRepo"

	MarketIdentityModeOAuthDelegated MarketIdentityMode = "oauthDelegated"
)

type AssociationMode string

const (
	AssociationRequireApproval AssociationMode = "RequireApproval"

	AssociationSkip AssociationMode = "Skip"
)

type VaultRef struct {
	Mount string `json:"mount"`

	Path string `json:"path"`
}

type MarketIdentitySpec struct {
	Mode MarketIdentityMode `json:"mode"`

	Handle string `json:"handle,omitempty"`

	PdsUrl string `json:"pdsUrl,omitempty"`

	Association AssociationMode `json:"association,omitempty"`

	AssociateWith string `json:"associateWith,omitempty"`

	ApprovedAssociations []string `json:"approvedAssociations,omitempty"`

	RejectedAssociations []string `json:"rejectedAssociations,omitempty"`

	AssociationWatch string `json:"associationWatch,omitempty"`

	KeysRef *VaultRef `json:"keysRef,omitempty"`

	SessionRef *VaultRef `json:"sessionRef,omitempty"`
}

type MarketIdentityPhase string

const (
	MarketIdentityPhasePending MarketIdentityPhase = "Pending"

	MarketIdentityPhaseAwaitingScan MarketIdentityPhase = "AwaitingScan"

	MarketIdentityPhaseAwaitingAuthorization MarketIdentityPhase = "AwaitingAuthorization"

	MarketIdentityPhaseAwaitingApproval MarketIdentityPhase = "AwaitingApproval"

	MarketIdentityPhaseReady MarketIdentityPhase = "Ready"

	MarketIdentityPhaseSessionExpired MarketIdentityPhase = "SessionExpired"

	MarketIdentityPhaseFailed MarketIdentityPhase = "Failed"
)

type PendingActionKind string

const (
	PendingActionScanQr PendingActionKind = "scanQr"

	PendingActionVisitUrl PendingActionKind = "visitUrl"

	PendingActionApproveAssociation PendingActionKind = "approveAssociation"

	PendingActionSignInAgain PendingActionKind = "signInAgain"
)

type MarketIdentityPendingAction struct {
	Kind PendingActionKind `json:"kind"`

	Url string `json:"url,omitempty"`

	CallerDid string `json:"callerDid,omitempty"`

	Handle string `json:"handle,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`
}

type MarketIdentityStatus struct {
	Phase MarketIdentityPhase `json:"phase,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Did string `json:"did,omitempty"`

	Handle string `json:"handle,omitempty"`

	PdsUrl string `json:"pdsUrl,omitempty"`

	ServiceDid string `json:"serviceDid,omitempty"`

	PendingAction *MarketIdentityPendingAction `json:"pendingAction,omitempty"`

	Associations []string `json:"associations,omitempty"`

	SessionRef *VaultRef `json:"sessionRef,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	ConditionSignedIn = "SignedIn"

	ConditionAssociated = "Associated"
)

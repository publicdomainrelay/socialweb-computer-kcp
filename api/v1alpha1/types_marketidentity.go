package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
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

	MarketIdentityModeOAuthSessionProxy MarketIdentityMode = "oauthSessionProxy"
)

type AssociationMode string

const (
	AssociationRequireApproval AssociationMode = "RequireApproval"

	AssociationSkip AssociationMode = "Skip"
)

type MarketIdentitySpec struct {
	Mode MarketIdentityMode `json:"mode"`

	Handle string `json:"handle,omitempty"`

	PdsURL string `json:"pdsUrl,omitempty" apigen:"userEndpoint"`

	Did string `json:"did,omitempty"`

	Association AssociationMode `json:"association,omitempty"`

	AssociateWith string `json:"associateWith,omitempty"`

	ApprovedAssociations []string `json:"approvedAssociations,omitempty" apigen:"listType=set"`

	RejectedAssociations []string `json:"rejectedAssociations,omitempty" apigen:"listType=set"`

	AssociationWatch string `json:"associationWatch,omitempty"`

	AssociationService string `json:"associationService,omitempty"`

	KeysRef *commonv1.SecretRef `json:"keysRef,omitempty"`

	SessionRef *commonv1.SecretRef `json:"sessionRef,omitempty"`

	SessionScope string `json:"sessionScope,omitempty"`

	OAuthClientID string `json:"oauthClientID,omitempty"`

	OAuthRedirectURI string `json:"oauthRedirectURI,omitempty" apigen:"userEndpoint"`

	CARef *commonv1.ObjectRef `json:"caRef,omitempty"`
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

const FieldManagerMarketIdentityApproval = "socialweb.computer/approve"

type PendingActionKind string

const (
	PendingActionScanQr PendingActionKind = "scanQr"

	PendingActionVisitUrl PendingActionKind = "visitUrl"

	PendingActionApproveAssociation PendingActionKind = "approveAssociation"

	PendingActionSignInAgain PendingActionKind = "signInAgain"
)

type MarketIdentityPendingAction struct {
	Kind PendingActionKind `json:"kind"`

	URL string `json:"url,omitempty"`

	CallerDid string `json:"callerDid,omitempty"`

	Handle string `json:"handle,omitempty"`

	ExpiresAt string `json:"expiresAt,omitempty"`
}

type MarketIdentityStatus struct {
	Phase MarketIdentityPhase `json:"phase,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	Did string `json:"did,omitempty"`

	Handle string `json:"handle,omitempty"`

	PdsURL string `json:"pdsUrl,omitempty"`

	ServiceDid string `json:"serviceDid,omitempty"`

	PendingAction *MarketIdentityPendingAction `json:"pendingAction,omitempty"`

	Associations []string `json:"associations,omitempty"`

	SessionRef *commonv1.SecretRef `json:"sessionRef,omitempty"`

	OAuthClientID string `json:"oauthClientID,omitempty"`

	OAuthRedirectURI string `json:"oauthRedirectURI,omitempty"`

	Message string `json:"message,omitempty"`

	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

const (
	ConditionSignedIn = "SignedIn"

	ConditionAssociated = "Associated"
)

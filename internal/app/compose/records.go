package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const (
	VouchNSID = "sh.tangled.graph.vouch"

	OperatorRepoName = "operator"

	OperatorVouchPrefix = "operator-vouch-"

	OperatorRecordLabel = "apps.socialweb.computer/record-repo"

	OperatorSessionPath = "identity/operator/pds-session"

	operatorRecordLinkSet = "operator-records"
)

type OwnerSession struct {
	Service    string `json:"service"`
	Identifier string `json:"identifier"`
	Password   string `json:"password"`
}

func ReadOwnerSessions(path string) (map[string]OwnerSession, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sessions map[string]OwnerSession
	if err := json.Unmarshal(b, &sessions); err != nil {
		return nil, fmt.Errorf("compose: the owner sessions file %s: %w", path, err)
	}
	return sessions, nil
}

func FirstOwnerSession(path string) (string, OwnerSession, error) {
	sessions, err := ReadOwnerSessions(path)
	if err != nil {
		return "", OwnerSession{}, err
	}
	owners := make([]string, 0, len(sessions))
	for did := range sessions {
		owners = append(owners, did)
	}
	if len(owners) == 0 {
		return "", OwnerSession{}, fmt.Errorf("compose: the owner sessions file %s holds no session", path)
	}
	slices.Sort(owners)
	return owners[0], sessions[owners[0]], nil
}

func OperatorRepoRef() *commonv1.ObjectRef {
	return &commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindAtprotoRepo, Name: OperatorRepoName}
}

func (c *Composer) ApplyOperatorVouch(ctx context.Context, r Root, ws, app, subject, at string) error {
	record, err := json.Marshal(map[string]any{"$type": VouchNSID, "subject": subject, "createdAt": at})
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: OperatorVouchPrefix + app, Labels: map[string]string{OperatorRecordLabel: OperatorRepoName}},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: OperatorRepoRef(), Collection: VouchNSID, Rkey: subject, Record: record}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

const AssociationRecordName = trust.MirrorRKey

func (c *Composer) ApplyAssociationRecord(ctx context.Context, r Root, ws string, repo commonv1.ObjectRef, identity, service string) error {
	from := func(path string) map[string]any {
		return map[string]any{"valueFrom": map[string]any{"kind": KindMarketIdentity, "name": identity, "fieldPath": path}}
	}
	record, err := json.Marshal(map[string]any{"challenge": from("status.associations[0]"), "keyId": from("status.did"),
		"service": service, "name": identity, "createdAt": from("metadata.creationTimestamp")})
	if err != nil {
		return err
	}
	obj := &v1alpha1.AtprotoRecord{ObjectMeta: metav1.ObjectMeta{Name: AssociationRecordName},
		Spec: v1alpha1.AtprotoRecordSpec{RepoRef: &repo, Collection: BadgeBlueKeysNSID, Rkey: AssociationRecordName, Record: record,
			OnMissingReference: v1alpha1.MissingReferenceDelete}}
	return c.ApplyTyped(ctx, r, ws, AtprotoRecordGVR, v1alpha1.GroupVersion.String(), "AtprotoRecord", obj)
}

func OperatorRecordLinks(ws string, remotePDS commonv1.ObjectRef, did string, caRef *commonv1.ObjectRef) []Link {
	repo := AppRef(KindAtprotoRepo, OperatorRepoName, ws)
	remote := AppRef(KindRemoteRepo, OperatorRepoName, ws)
	records := exposurev1alpha1.ComponentLinkSelector{Group: v1alpha1.GroupName, Kind: "AtprotoRecord", Workspace: ws,
		MatchLabels: map[string]string{OperatorRecordLabel: OperatorRepoName}}
	links := []Link{
		SelectorLink(LinkName("records", OperatorRepoName), records, repo, Cap(atprotorepo.CapabilityWrite, VouchNSID)),
		RepoBackendLink(repo, remote),
	}
	identity := AppRef(KindMarketIdentity, OperatorRepoName, ws)
	if caRef != nil {
		links = append(links, TrustLink(identity, *caRef))
	}
	return append(links, RemoteRepoLinks(remote, remotePDS, identity, did)...)
}

func (c *Composer) ApplyOperatorRepo(ctx context.Context, r Root, ws string, op Operator) error {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return err
	}
	records, err := kube.List[v1alpha1.AtprotoRecord](ctx, ri, metav1.ListOptions{LabelSelector: OperatorRecordLabel + "=" + OperatorRepoName})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return nil
	}
	did := ""
	if id, err := c.Identity(ctx, ws, OperatorRepoName); err == nil {
		did = strings.TrimSpace(id.Status.Did)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if did == "" {
		if _, did, err = c.OperatorRepo(ctx, r, op); err != nil {
			return err
		}
	} else if strings.TrimSpace(op.PdsURL) != "" {
		if err := c.applyOperatorPDS(ctx, r, op); err != nil {
			return err
		}
	}
	vault := func() *commonv1.ObjectRef {
		return &commonv1.ObjectRef{Kind: v1alpha1.KindOpenBaoServer, Name: op.Vault, Workspace: r.Workspace}
	}
	identity := v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModePdsAccount, Handle: strings.TrimSpace(op.Identifier),
		PdsURL: strings.TrimRight(strings.TrimSpace(op.PdsURL), "/"), Association: v1alpha1.AssociationSkip,
		KeysRef:    &commonv1.SecretRef{Vault: vault(), Mount: op.Mount, Path: OperatorKeysPath},
		SessionRef: &commonv1.SecretRef{Vault: vault(), Mount: op.Mount, Path: OperatorSessionPath}}
	var caRef *commonv1.ObjectRef
	if op.CARef != nil && op.tls() {
		ca := *op.CARef
		caRef, identity.CARef = &ca, &ca
	}
	links := OperatorRecordLinks(ws, op.ref(r.Workspace), did, caRef)
	if err := c.AddLinks(ctx, r, operatorRecordLinkSet, links); err != nil {
		return err
	}
	if err := c.ApplyIdentitySpec(ctx, r, ws, OperatorRepoName, identity); err != nil {
		return err
	}
	pdsRef := op.ref(r.Workspace)
	remote := &v1alpha1.RemoteRepo{ObjectMeta: metav1.ObjectMeta{Name: OperatorRepoName}, Spec: v1alpha1.RemoteRepoSpec{DID: did, PdsRef: pdsRef,
		CredentialsRef: commonv1.CredentialsRef{ObjectRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindMarketIdentity, Name: OperatorRepoName}}}}
	if err := c.ApplyRepo(ctx, r, ws, OperatorRepoName, KindRemoteRepo, v1alpha1.RemoteRepoGVR, remote); err != nil {
		return err
	}
	return c.ApplyLinks(ctx, r, operatorRecordLinkSet, links, ws)
}

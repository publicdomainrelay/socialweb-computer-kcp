package compose

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const SessionOwnerLabel = "apps.socialweb.computer/session-owner"

type SessionOwner struct {
	DID string

	Vault string

	Scope string
}

type SessionOwnerState struct {
	PdsURL string

	Ready bool

	Message string
}

func (o SessionOwner) did() string { return strings.TrimSpace(o.DID) }

func (o SessionOwner) sessionPath() string { return "session/" + o.did() }

func (o SessionOwner) sessionScope() string {
	return "auth/" + AtprotoMountPath + "/" + o.sessionPath()
}

func (o SessionOwner) SessionRef(r Root) commonv1.SecretRef {
	return commonv1.SecretRef{Vault: &commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindVault, Name: o.Vault, Workspace: r.Workspace},
		Mount: "auth/" + AtprotoMountPath, Path: o.sessionPath()}
}

func ownerPdsName(pdsURL string) string {
	return "owner-" + Stamp(strings.TrimRight(strings.TrimSpace(pdsURL), "/"))
}

func sessionOwnerLabels() map[string]string {
	return map[string]string{SessionOwnerLabel: "true"}
}

func SessionOwnerLinks(r Root, ws, contracts string, o SessionOwner, pdsURL string) []Link {
	identity := AppRef(KindMarketIdentity, OwnerIdentityName(o.did()), ws)
	links := []Link{RefLink(identity, VaultRefIn(o.Vault, r.Workspace), Cap(vault.CapabilityAtprotoSession, o.sessionScope()))}
	if pdsURL = strings.TrimRight(strings.TrimSpace(pdsURL), "/"); pdsURL == "" {
		return links
	}
	repo := RepoRefIn(o.did(), ws)
	remote := AppRef(KindRemoteRepo, repo.Name, ws)
	links = append(links, RepoBackendLink(repo, remote))
	links = append(links, RemoteRepoLinks(remote, RemotePDSRefIn(ownerPdsName(pdsURL), ws), identity, o.did())...)
	return append(links, InfraLinks(contracts, repo)...)
}

func (c *Composer) SessionOwnerState(ctx context.Context, ws, did string) (SessionOwnerState, error) {
	name := OwnerIdentityName(did)
	id, err := c.Identity(ctx, ws, name)
	if apierrors.IsNotFound(err) {
		return SessionOwnerState{Message: "the MarketIdentity " + name + " is not applied yet"}, nil
	}
	if err != nil {
		return SessionOwnerState{}, err
	}
	return SessionOwnerStateOf(id, did), nil
}

func SessionOwnerStateOf(id *v1alpha1.MarketIdentity, did string) SessionOwnerState {
	name := OwnerIdentityName(did)
	if id.Spec.Mode != v1alpha1.MarketIdentityModeOAuthSessionProxy || strings.TrimSpace(id.Status.Did) != strings.TrimSpace(did) {
		return SessionOwnerState{Message: "the MarketIdentity " + name + " has not reported " + did + " yet"}
	}
	st := SessionOwnerState{Message: cmp.Or(id.Status.Message, string(id.Status.Phase)),
		Ready: id.Status.Phase == v1alpha1.MarketIdentityPhaseReady && id.Status.ObservedGeneration >= id.Generation}
	switch id.Status.Phase {
	case v1alpha1.MarketIdentityPhaseReady, v1alpha1.MarketIdentityPhaseSessionExpired:
		st.PdsURL = strings.TrimRight(strings.TrimSpace(id.Status.PdsURL), "/")
	}
	return st
}

const (
	ConditionSessionOwnersReady = "SessionOwnersReady"

	ReasonSessionOwnersReady = "Ready"

	ReasonSessionOwnerNotReady = "OwnerNotReady"
)

func SetSessionOwnersCondition(conditions *[]metav1.Condition, gen int64, states map[string]SessionOwnerState) {
	var waiting []string
	for _, did := range slices.Sorted(maps.Keys(states)) {
		if st := states[did]; !st.Ready {
			waiting = append(waiting, did+": "+st.Message)
		}
	}
	if len(waiting) == 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionSessionOwnersReady, Status: metav1.ConditionTrue,
			Reason: ReasonSessionOwnersReady, Message: fmt.Sprintf("%d tenant owner(s) publish Infra through the web app session", len(states)),
			ObservedGeneration: gen})
		return
	}
	meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionSessionOwnersReady, Status: metav1.ConditionFalse,
		Reason: ReasonSessionOwnerNotReady, Message: strings.Join(waiting, "; "), ObservedGeneration: gen})
}

func (c *Composer) ApplySessionOwner(ctx context.Context, r Root, ws string, o SessionOwner, pdsURL string) error {
	ref := o.SessionRef(r)
	name := OwnerIdentityName(o.did())
	identity := &v1alpha1.MarketIdentity{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: sessionOwnerLabels()},
		Spec: v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModeOAuthSessionProxy, Did: o.did(), Association: v1alpha1.AssociationSkip, SessionRef: &ref,
			SessionScope: strings.TrimSpace(o.Scope)}}
	if err := c.ApplyTyped(ctx, r, ws, MarketIdentityGVR, v1alpha1.GroupVersion.String(), KindMarketIdentity, identity); err != nil {
		return err
	}
	if pdsURL = strings.TrimRight(strings.TrimSpace(pdsURL), "/"); pdsURL == "" {
		return nil
	}
	remote := pdsv1alpha1.RemotePDSSpec{URL: pdsURL, DID: pdsServiceDID(pdsURL)}
	if _, err := c.ApplyRemotePDS(ctx, r, ws, &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: ownerPdsName(pdsURL), Labels: sessionOwnerLabels()}, Spec: remote}); err != nil {
		return err
	}
	repo := RepoRefIn(o.did(), ws).Name
	spec := v1alpha1.RemoteRepoSpec{DID: o.did(), PdsRef: RemotePDSRefIn(ownerPdsName(pdsURL), ""),
		CredentialsRef: commonv1.CredentialsRef{ObjectRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindMarketIdentity, Name: name}}}
	return c.applyRepo(ctx, r, ws, repo, KindRemoteRepo, v1alpha1.RemoteRepoGVR,
		&v1alpha1.RemoteRepo{ObjectMeta: metav1.ObjectMeta{Name: repo, Labels: sessionOwnerLabels()}, Spec: spec}, sessionOwnerLabels())
}

func (c *Composer) SessionOwners(ctx context.Context, ws string) ([]string, error) {
	ri, err := c.Client(ws, MarketIdentityGVR)
	if err != nil {
		return nil, err
	}
	items, err := kube.List[v1alpha1.MarketIdentity](ctx, ri, metav1.ListOptions{LabelSelector: SessionOwnerLabel + "=true"})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, id := range items {
		if did := strings.TrimSpace(id.Spec.Did); did != "" && id.Spec.Mode == v1alpha1.MarketIdentityModeOAuthSessionProxy {
			out = append(out, did)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (c *Composer) InfraRecordRepos(ctx context.Context, ws string) (map[string]bool, error) {
	ri, err := c.Client(ws, AtprotoRecordGVR)
	if err != nil {
		return nil, err
	}
	items, err := kube.List[v1alpha1.AtprotoRecord](ctx, ri, metav1.ListOptions{LabelSelector: v1alpha1.InfraKindLabel})
	if apierrors.IsNotFound(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, rec := range items {
		if rec.Spec.RepoRef != nil && strings.TrimSpace(rec.Spec.RepoRef.Name) != "" {
			out[strings.TrimSpace(rec.Spec.RepoRef.Name)] = true
		}
	}
	return out, nil
}

func (c *Composer) PruneSessionOwners(ctx context.Context, ws string, keep []string) error {
	identities, repos := map[string]bool{}, map[string]bool{}
	for _, did := range keep {
		identities[OwnerIdentityName(did)] = true
		repos[atprotorepo.RepoName(strings.TrimSpace(did))] = true
	}
	selector := SessionOwnerLabel + "=true"
	if err := c.Prune(ctx, ws, v1alpha1.AtprotoRepoGVR, selector, repos); err != nil {
		return err
	}
	if err := c.Prune(ctx, ws, v1alpha1.RemoteRepoGVR, selector, repos); err != nil {
		return err
	}
	pdses, err := c.sessionOwnerPDSes(ctx, ws, selector)
	if err != nil {
		return err
	}
	if err := c.Prune(ctx, ws, RemotePDSGVR, selector, pdses); err != nil {
		return err
	}
	return c.Prune(ctx, ws, MarketIdentityGVR, selector, identities)
}

func (c *Composer) sessionOwnerPDSes(ctx context.Context, ws, selector string) (map[string]bool, error) {
	ri, err := c.Client(ws, v1alpha1.RemoteRepoGVR)
	if err != nil {
		return nil, err
	}
	items, err := kube.List[v1alpha1.RemoteRepo](ctx, ri, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, repo := range items {
		if repo.DeletionTimestamp == nil && strings.TrimSpace(repo.Spec.PdsRef.Name) != "" {
			out[strings.TrimSpace(repo.Spec.PdsRef.Name)] = true
		}
	}
	return out, nil
}

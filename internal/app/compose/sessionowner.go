package compose

import (
	"context"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func (c *Composer) SessionOwnerPds(ctx context.Context, ws, did string) (string, error) {
	id, err := c.Identity(ctx, ws, OwnerIdentityName(did))
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if id.Spec.Mode != v1alpha1.MarketIdentityModeOAuthSessionProxy || id.Status.Phase != v1alpha1.MarketIdentityPhaseReady ||
		strings.TrimSpace(id.Status.Did) != strings.TrimSpace(did) {
		return "", nil
	}
	return strings.TrimRight(strings.TrimSpace(id.Status.PdsURL), "/"), nil
}

func (c *Composer) ApplySessionOwner(ctx context.Context, r Root, ws string, o SessionOwner, pdsURL string) error {
	ref := o.SessionRef(r)
	name := OwnerIdentityName(o.did())
	identity := &v1alpha1.MarketIdentity{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: sessionOwnerLabels()},
		Spec: v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModeOAuthSessionProxy, Did: o.did(), Association: v1alpha1.AssociationSkip, SessionRef: &ref}}
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

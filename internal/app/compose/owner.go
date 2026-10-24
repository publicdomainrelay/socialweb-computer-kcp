package compose

import (
	"context"
	"errors"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

type Owner struct {
	DID string

	PdsURL string

	Identifier string

	Password string

	CARef *commonv1.ObjectRef

	Vault string

	Mount string
}

func OwnerIdentityName(did string) string {
	return "owner-" + Stamp(strings.TrimSpace(did))
}

func (o Owner) pdsURL() string {
	return strings.TrimRight(strings.TrimSpace(o.PdsURL), "/")
}

func (o Owner) keysPath() string {
	return "identity/" + OwnerIdentityName(o.DID) + "/keys"
}

func (o Owner) sessionPath() string {
	return "identity/" + OwnerIdentityName(o.DID) + "/pds-session"
}

func (o Owner) pdsName() string {
	return ownerPdsName(o.pdsURL())
}

func (o Owner) tls() bool {
	return strings.HasPrefix(o.pdsURL(), "https://")
}

func OwnerRecordSources(ws string, network *v1alpha1.MarketNetworkValue) []commonv1.ObjectRef {
	refs := WatchEventSources(SourceInput{Network: network})
	for i := range refs {
		refs[i].Workspace = ws
	}
	return refs
}

func RecordWatchStreamLinks(contracts string, sources []commonv1.ObjectRef) []Link {
	sel := streamSelector(contracts, nil, []exposurev1alpha1.ComponentLinkSelectorRequirement{
		{Key: eventstream.RecordWatchLabel, Operator: exposurev1alpha1.ComponentLinkSelectorExists}})
	out := make([]Link, 0, len(sources))
	for _, s := range sources {
		out = append(out, SelectorLink(LinkName("infra-watch", contracts, s.Workspace, s.Name), sel, s, Cap(events.CapabilitySubscribe, events.ScopeAll)))
	}
	return out
}

func OwnerRepoLinks(ws, contracts string, o Owner) []Link {
	repo := RepoRefIn(o.DID, ws)
	remote := AppRef(KindRemoteRepo, repo.Name, ws)
	identity := AppRef(KindMarketIdentity, OwnerIdentityName(o.DID), ws)
	pdsRef := RemotePDSRefIn(o.pdsName(), ws)
	links := append([]Link{RepoBackendLink(repo, remote)}, RemoteRepoLinks(remote, pdsRef, identity, o.DID)...)
	if o.CARef != nil && o.tls() {
		links = append(links, TrustLink(identity, *o.CARef), TrustLink(pdsRef, *o.CARef))
	}
	return append(links, InfraLinks(contracts, repo)...)
}

func (c *Composer) ApplyOwnerRepo(ctx context.Context, r Root, ws string, o Owner) error {
	if strings.TrimSpace(o.DID) == "" || o.pdsURL() == "" || strings.TrimSpace(o.Identifier) == "" || o.Password == "" {
		return errors.New("compose: the owner account names no DID, PDS URL, identifier or password")
	}
	kv, err := c.KV(ctx, r, o.Vault, o.Mount+"/identity/*")
	if err != nil {
		return err
	}
	data, err := readKV(ctx, kv, o.Mount, o.keysPath())
	if err != nil {
		return err
	}
	if data["password"] != o.Password {
		data["password"] = o.Password
		if err := kv.Write(ctx, o.Mount, o.keysPath(), data); err != nil {
			return err
		}
	}
	remote := pdsv1alpha1.RemotePDSSpec{URL: o.pdsURL(), DID: pdsServiceDID(o.pdsURL())}
	vault := &commonv1.ObjectRef{Kind: v1alpha1.KindOpenBaoServer, Name: o.Vault, Workspace: r.Workspace}
	identity := v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModePdsAccount, Handle: strings.TrimSpace(o.Identifier), PdsURL: o.pdsURL(),
		Association: v1alpha1.AssociationSkip,
		KeysRef:     &commonv1.SecretRef{Vault: vault, Mount: o.Mount, Path: o.keysPath()},
		SessionRef:  &commonv1.SecretRef{Vault: vault, Mount: o.Mount, Path: o.sessionPath()}}
	if o.CARef != nil && o.tls() {
		ca := *o.CARef
		remote.TLS = pdsv1alpha1.RemotePDSTLS{Enabled: true, CARef: &ca}
		identity.CARef = &ca
	}
	if _, err := c.ApplyRemotePDS(ctx, r, ws, &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: o.pdsName()}, Spec: remote}); err != nil {
		return err
	}
	name := OwnerIdentityName(o.DID)
	if err := c.ApplyIdentitySpec(ctx, r, ws, name, identity); err != nil {
		return err
	}
	repo := RepoRefIn(o.DID, ws).Name
	spec := v1alpha1.RemoteRepoSpec{DID: strings.TrimSpace(o.DID), PdsRef: RemotePDSRefIn(o.pdsName(), ""),
		CredentialsRef: commonv1.CredentialsRef{ObjectRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindMarketIdentity, Name: name}}}
	return c.ApplyRepo(ctx, r, ws, repo, KindRemoteRepo, v1alpha1.RemoteRepoGVR, &v1alpha1.RemoteRepo{ObjectMeta: metav1.ObjectMeta{Name: repo}, Spec: spec})
}

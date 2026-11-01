package marketintents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	market "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	KindSshServer = "SshServer"

	sessionOwnerMount = "auth/atproto"

	rootLabelPrefix = "apps.socialweb.computer/root"
)

const maxSessionOwnerDID = 2048

var sessionOwnerDIDPattern = regexp.MustCompile(`^did:[a-z]+:[A-Za-z0-9._:%-]+$`)

func sessionOwnerDID(did string) bool {
	return len(did) <= maxSessionOwnerDID && sessionOwnerDIDPattern.MatchString(did)
}

type sessionOwners struct {
	h *handle
}

var _ market.SessionOwnerIntents = sessionOwners{}

func (c *Controller) openSessionOwners(ctx context.Context, to commonv1.ObjectRef, scope []string) (market.SessionOwnerIntents, error) {
	if !strings.EqualFold(strings.TrimSpace(to.Kind), KindSshServer) {
		return nil, fmt.Errorf("marketintents: session owners serve an SshServer, not %s", to.String())
	}
	opened, err := c.open(ctx, to, scope)
	if err != nil {
		return nil, err
	}
	return sessionOwners{h: opened.(*handle)}, nil
}

func (s sessionOwners) server(ctx context.Context, cl client.Client) (*v1alpha1.SshServer, error) {
	doc, err := kube.GetWith(ctx, cl, kindOf(KindSshServer), types.NamespacedName{Name: s.h.owner.Name})
	if err != nil {
		return nil, fmt.Errorf("marketintents: reading SshServer %s: %w", s.h.owner.Name, err)
	}
	return kube.Decode[v1alpha1.SshServer](doc)
}

func (s sessionOwners) EnsureSessionOwner(ctx context.Context, did string) error {
	did = strings.TrimSpace(did)
	if !sessionOwnerDID(did) {
		return fmt.Errorf("marketintents: %q is not a DID", did)
	}
	cl, err := s.h.client()
	if err != nil {
		return err
	}
	server, err := s.server(ctx, cl)
	if err != nil {
		return err
	}
	if server.Spec.VaultRef == nil || strings.TrimSpace(server.Spec.VaultRef.Name) == "" {
		return errors.New("marketintents: the SshServer names no vault for the session")
	}
	name := market.SessionOwnerName(market.SessionOwnerIdentityPrefix, did)
	found, err := s.h.exists(ctx, market.KindMarketIdentity, name)
	if err != nil || found {
		return err
	}
	list := &metav1.PartialObjectMetadataList{}
	list.SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(market.KindMarketIdentity + "List"))
	if err := cl.List(ctx, list, client.MatchingLabels{market.SessionOwnerLabel: "true"}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("marketintents: listing the session owners: %w", err)
	}
	limit := int(server.Spec.MaxAccounts)
	if limit <= 0 {
		limit = market.DefaultMaxSessionOwners
	}
	if len(list.Items) >= limit {
		return fmt.Errorf("%w (%d)", market.ErrSessionOwnersFull, limit)
	}
	labels := map[string]string{market.SessionOwnerLabel: "true"}
	for key, value := range server.Labels {
		if strings.HasPrefix(key, rootLabelPrefix) {
			labels[key] = value
		}
	}
	vault := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: server.Spec.VaultRef.Name, Workspace: s.h.owner.Workspace}
	spec := v1alpha1.MarketIdentitySpec{Mode: v1alpha1.MarketIdentityModeOAuthSessionProxy, Did: did, Association: v1alpha1.AssociationSkip,
		SessionRef: &commonv1.SecretRef{Vault: &vault, Mount: sessionOwnerMount, Path: "session/" + did}}
	err = s.h.create(ctx, market.KindMarketIdentity, metav1.ObjectMeta{Name: name, Labels: labels}, spec, s.h.manager)
	if errors.Is(err, market.ErrExists) {
		return nil
	}
	return err
}

func (s sessionOwners) DeleteSessionOwner(ctx context.Context, did string) error {
	did = strings.TrimSpace(did)
	if !sessionOwnerDID(did) {
		return fmt.Errorf("marketintents: %q is not a DID", did)
	}
	cl, err := s.h.client()
	if err != nil {
		return err
	}
	name := market.SessionOwnerName(market.SessionOwnerIdentityPrefix, did)
	doc, err := kube.GetWith(ctx, cl, kindOf(market.KindMarketIdentity), types.NamespacedName{Name: name})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	meta := metadataOf(doc)
	labels, _ := meta["labels"].(map[string]any)
	if labels[market.SessionOwnerLabel] != "true" {
		return fmt.Errorf("marketintents: %s is not a session owner", name)
	}
	err = kube.DeleteWith(ctx, cl, kindOf(market.KindMarketIdentity), types.NamespacedName{Name: name})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

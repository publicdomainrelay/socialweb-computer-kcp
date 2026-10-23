package remoterepo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/identity"
)

const IdentityKind = "MarketIdentity"

var IdentityGroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: IdentityKind}

type IdentityReader func(ctx context.Context, workspace, name string) (*v1alpha1.MarketIdentity, error)

type VaultStore func(workspace string, ref commonv1.SecretRef) identity.Store

type CredentialOptions struct {
	Bus *bus.Bus

	Identities IdentityReader

	Vault VaultStore

	HTTP *http.Client
}

type credentials struct {
	opts CredentialOptions

	mu sync.Mutex

	held map[string]*identity.Authenticator
}

func ProvideCredentials(opts CredentialOptions) {
	c := &credentials{opts: opts, held: map[string]*identity.Authenticator{}}
	bus.Provide(opts.Bus, IdentityGroupKind, pds.CapabilitySession, c.open)
}

func (c *credentials) open(ctx context.Context, to commonv1.ObjectRef, scope []string) (pds.Authenticator, error) {
	did, err := pds.AccountDID(scope)
	if err != nil {
		return nil, err
	}
	if c.opts.Identities == nil || c.opts.Vault == nil {
		return nil, fmt.Errorf("%w: no MarketIdentity credentials source is configured", pds.ErrUnsupported)
	}
	id, err := c.opts.Identities(ctx, to.Workspace, to.Name)
	if err != nil {
		return nil, fmt.Errorf("%w: the MarketIdentity %s: %v", pds.ErrNotFound, to.String(), err)
	}
	if strings.TrimSpace(id.Status.Did) != did {
		return nil, fmt.Errorf("pds: the MarketIdentity %s signs for %q, not %s", to.String(), id.Status.Did, did)
	}
	ref := id.Status.SessionRef
	if ref == nil {
		ref = id.Spec.SessionRef
	}
	if ref == nil {
		return nil, fmt.Errorf("%w: the MarketIdentity %s names no session", pds.ErrNotFound, to.String())
	}
	key := strings.Join([]string{to.Workspace, strings.ToLower(to.Name), did, ref.Mount, ref.Path, ref.Key}, "/")
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.held[key]; ok {
		return held, nil
	}
	clientID := id.Status.OAuthClientID
	if clientID == "" {
		clientID = id.Spec.OAuthClientID
	}
	auth, err := identity.New(ctx, identity.Config{DID: did, PDSURL: id.Status.PdsURL, ClientID: clientID,
		Store: c.opts.Vault(to.Workspace, *ref), HTTP: c.opts.HTTP})
	if err != nil {
		return nil, errors.Join(pds.ErrNotFound, err)
	}
	c.held[key] = auth
	return auth, nil
}

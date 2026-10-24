package atproto

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type heldAccount struct {
	open sync.Mutex

	mode v1alpha1.MarketIdentityMode

	ref commonv1.SecretRef

	auth *identity.Authenticator
}

type heldAccounts struct {
	mu sync.Mutex

	accounts map[string]*heldAccount
}

func (h *heldAccounts) get(workspace string, id *v1alpha1.MarketIdentity) *heldAccount {
	key := qrKey(workspace, id.Name)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.accounts == nil {
		h.accounts = map[string]*heldAccount{}
	}
	if held, ok := h.accounts[key]; ok && held.mode == id.Spec.Mode {
		return held
	}
	held := &heldAccount{mode: id.Spec.Mode}
	if id.Spec.Mode == v1alpha1.MarketIdentityModePdsAccount {
		held.auth = identity.NewBearer()
	} else {
		held.auth = identity.NewOAuth()
	}
	if previous, ok := h.accounts[key]; ok {
		previous.auth.Drop()
	}
	h.accounts[key] = held
	return held
}

func (h *heldAccounts) forget(workspace, name string) {
	key := qrKey(workspace, name)
	h.mu.Lock()
	held, ok := h.accounts[key]
	delete(h.accounts, key)
	h.mu.Unlock()
	if ok {
		held.auth.Drop()
	}
}

func (a *Accounts) Authenticator(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (*identity.Authenticator, error) {
	held := a.held.get(workspace, id)
	switch id.Spec.Mode {
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
		if held.auth.Session() == nil {
			status := id.Status
			if _, _, err := a.restoreOAuthSession(ctx, workspace, id, &status); err != nil {
				return nil, err
			}
		}
	case v1alpha1.MarketIdentityModePdsAccount:
		if held.auth.Bearer() == nil {
			if err := a.holdBearer(ctx, workspace, id, oauthSessionRef(id, &id.Status), strings.TrimSpace(id.Status.Did),
				firstNonEmpty(id.Status.PdsURL, id.Spec.PdsURL)); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("marketidentity: mode %q holds no account session", id.Spec.Mode)
	}
	return held.auth, nil
}

func (a *Accounts) holdBearer(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, ref *commonv1.SecretRef, did, pdsURL string) error {
	if ref == nil {
		return errNoSessionRef
	}
	vault := a.vaultFor(workspace)
	if vault == nil {
		return errors.New("marketidentity: no vault reads the pds account session")
	}
	if did == "" {
		return errors.New("marketidentity: the pds account has no DID yet")
	}
	held := a.held.get(workspace, id)
	held.open.Lock()
	defer held.open.Unlock()
	if held.auth.Bearer() != nil && held.ref == *ref && held.auth.DID() == did {
		return nil
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return err
	}
	held.ref = *ref
	held.auth.HoldBearer(did, identity.NewBearerTokens(secretStore{vault: vault, ref: *ref}, pdsURL, client))
	return nil
}

func (a *Accounts) sessionHTTP(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (*http.Client, error) {
	tlsConfig, err := a.tlsFor(ctx, workspace, id)
	if err != nil {
		return nil, err
	}
	base := a.oauthHTTP()
	if tlsConfig == nil {
		return base, nil
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	if base != nil {
		client.Timeout = base.Timeout
	}
	return client, nil
}

func (a *Accounts) holdPdsAccount(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, ref *commonv1.SecretRef, did, pdsURL string) {
	if err := a.holdBearer(ctx, workspace, id, ref, did, pdsURL); err != nil {
		a.log.Warn("the pds account session is not held", "workspace", workspace, "identity", id.Name, "err", err.Error())
	}
}

type secretStore struct {
	vault SecretStore

	ref commonv1.SecretRef
}

var _ identity.Store = secretStore{}

func (s secretStore) Read(ctx context.Context) (map[string]any, error) {
	return s.vault.Get(ctx, s.ref)
}

func (s secretStore) Write(ctx context.Context, data map[string]any) error {
	return s.vault.Put(ctx, s.ref, data)
}

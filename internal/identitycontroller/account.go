package identitycontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

type pdsAccountCredential struct {
	handle string

	password string

	pdsURL string
}

func (c *Controller) reconcilePdsAccount(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity,
	generation int64, status v1alpha1.MarketIdentityStatus) {
	ref := oauthSessionRef(id, &status)
	if ref != nil {
		status.SessionRef = ref
	}
	credential, err := c.pdsAccountCredential(ctx, id)
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "MissingCredential", err.Error())
		return
	}
	if ref != nil {
		if session, ok := c.storedPdsSession(ctx, *ref, credential.pdsURL); ok {
			c.signedIn(ctx, dyn, id, generation, status, session.DID.String(), firstNonEmpty(session.Handle, credential.handle), credential.pdsURL, ref)
			return
		}
	}
	pds, err := c.remotePDS(credential.pdsURL, nil)
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "PdsUnavailable", err.Error())
		return
	}
	session, err := pds.CreateSession(ctx, credential.handle, credential.password)
	if err != nil {
		c.failed(ctx, dyn, id, generation, status, "SignInFailed", fmt.Sprintf("com.atproto.server.createSession at %s: %v", credential.pdsURL, err))
		return
	}
	if ref != nil {
		if err := c.savePdsSession(ctx, *ref, credential.pdsURL, session); err != nil {
			c.pending(ctx, dyn, id, generation, status, "SessionStoreFailed", err.Error())
			return
		}
	}
	c.signedIn(ctx, dyn, id, generation, status, session.DID.String(), firstNonEmpty(session.Handle, credential.handle), credential.pdsURL, ref)
}

func (c *Controller) pdsAccountCredential(ctx context.Context, id *v1alpha1.MarketIdentity) (pdsAccountCredential, error) {
	if id.Spec.KeysRef == nil {
		return pdsAccountCredential{}, errors.New("mode pdsAccount names no spec.keysRef; the password stays in the vault and the controller does not fall back to a local repo")
	}
	if c.vault == nil {
		return pdsAccountCredential{}, errors.New("mode pdsAccount has no vault to read spec.keysRef")
	}
	data, err := c.vault.Get(ctx, *id.Spec.KeysRef)
	if err != nil {
		return pdsAccountCredential{}, fmt.Errorf("reading spec.keysRef %s/%s: %w", id.Spec.KeysRef.Mount, id.Spec.KeysRef.Path, err)
	}
	credential := pdsAccountCredential{
		handle:   firstNonEmpty(id.Spec.Handle, firstString(data, "handle", "identifier", "user", "username")),
		password: firstString(data, "password", "appPassword", "app_password"),
		pdsURL:   firstNonEmpty(id.Spec.PdsURL, firstString(data, "pdsUrl", "pds_url", "pds")),
	}
	switch {
	case credential.handle == "":
		return pdsAccountCredential{}, errors.New("mode pdsAccount has no handle in spec.handle or spec.keysRef")
	case credential.password == "":
		return pdsAccountCredential{}, errors.New("mode pdsAccount has no password in spec.keysRef; it fails closed")
	case credential.pdsURL == "":
		return pdsAccountCredential{}, errors.New("mode pdsAccount has no pdsUrl in spec.pdsUrl or spec.keysRef")
	}
	return credential, nil
}

func (c *Controller) storedPdsSession(ctx context.Context, ref v1alpha1.VaultRef, pdsURL string) (pdsclient.Session, bool) {
	if c.vault == nil {
		return pdsclient.Session{}, false
	}
	data, err := c.vault.Get(ctx, ref)
	if err != nil {
		return pdsclient.Session{}, false
	}
	access := firstString(data, "accessJwt", "accessToken", "access_token")
	if !oauthclient.AccessTokenLive(access) {
		return pdsclient.Session{}, false
	}
	tokens := pdsclient.TokenSourceFunc(func(ctx context.Context) (string, error) { return access, nil })
	pds, err := c.remotePDS(pdsURL, tokens)
	if err != nil {
		return pdsclient.Session{}, false
	}
	session, err := pds.GetSession(ctx)
	if err != nil {
		return pdsclient.Session{}, false
	}
	session.AccessJWT = access
	session.RefreshJWT = firstString(data, "refreshJwt", "refreshToken", "refresh_token")
	return session, true
}

func (c *Controller) savePdsSession(ctx context.Context, ref v1alpha1.VaultRef, pdsURL string, session pdsclient.Session) error {
	if c.vault == nil {
		return errors.New("identitycontroller: no vault writes the pds account session")
	}
	return c.vault.Put(ctx, ref, map[string]any{
		"did":           session.DID.String(),
		"handle":        session.Handle,
		"pds":           pdsURL,
		"host_url":      pdsURL,
		"accessJwt":     session.AccessJWT,
		"refreshJwt":    session.RefreshJWT,
		"access_token":  session.AccessJWT,
		"refresh_token": session.RefreshJWT,
	})
}

func (c *Controller) remotePDS(endpoint string, tokens pdsclient.TokenSource) (pdsclient.PDS, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("identitycontroller: the remote PDS has no URL")
	}
	if !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("identitycontroller: the remote PDS %q is not an absolute URL", endpoint)
	}
	r := c.resolver
	cfg := pdsclient.RemoteConfig{BaseURL: endpoint, Tokens: tokens, Timeout: requestTimeout}
	if r != nil {
		cfg.HTTPClient = r.HTTPClient
		cfg.TLS = r.TLS
		cfg.DPoP = r.DPoP
		if r.Timeout > 0 {
			cfg.Timeout = r.Timeout
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = c.http
	}
	return pdsclient.NewRemote(cfg)
}

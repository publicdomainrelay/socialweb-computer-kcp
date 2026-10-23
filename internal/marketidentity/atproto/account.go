package atproto

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type pdsAccountCredential struct {
	handle string

	password string

	pdsURL string
}

var errVaultRead = errors.New("marketidentity: reading the vault")

func (a *Accounts) pdsAccount(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity,
	generation int64, status v1alpha1.MarketIdentityStatus) (Result, error) {
	vault := a.vaultFor(workspace)
	ref := oauthSessionRef(id, &status)
	if ref != nil {
		status.SessionRef = ref
	}
	credential, err := pdsAccountCredentialOf(ctx, vault, id)
	if errors.Is(err, errVaultRead) {
		return failed(status, generation, "MissingCredential", err.Error()), err
	}
	if err != nil {
		return failed(status, generation, "MissingCredential", err.Error()), nil
	}
	tlsConfig, err := a.tlsFor(ctx, workspace, id)
	if err != nil {
		return pending(status, generation, "TrustUnavailable", err.Error()), err
	}
	if ref != nil {
		if session, ok := a.storedPdsSession(ctx, vault, *ref, credential.pdsURL, tlsConfig); ok {
			return signedIn(status, generation, id, session.DID.String(), firstNonEmpty(session.Handle, credential.handle), credential.pdsURL, ref,
				session.AccessJWT), nil
		}
	}
	pds, err := a.remotePDS(credential.pdsURL, nil, tlsConfig)
	if err != nil {
		return failed(status, generation, "PdsUnavailable", err.Error()), nil
	}
	session, err := pds.CreateSession(ctx, credential.handle, credential.password)
	if err != nil {
		message := fmt.Sprintf("com.atproto.server.createSession at %s: %v", credential.pdsURL, err)
		return failed(status, generation, "SignInFailed", message), errors.New(message)
	}
	if ref != nil {
		if err := savePdsSession(ctx, vault, *ref, credential.pdsURL, session); err != nil {
			return pending(status, generation, "SessionStoreFailed", err.Error()), err
		}
	}
	return signedIn(status, generation, id, session.DID.String(), firstNonEmpty(session.Handle, credential.handle), credential.pdsURL, ref,
		session.AccessJWT), nil
}

func pdsAccountCredentialOf(ctx context.Context, vault SecretStore, id *v1alpha1.MarketIdentity) (pdsAccountCredential, error) {
	if id.Spec.KeysRef == nil {
		return pdsAccountCredential{}, errors.New("mode pdsAccount names no spec.keysRef; the password stays in the vault and the controller does not fall back to a local repo")
	}
	if vault == nil {
		return pdsAccountCredential{}, errors.New("mode pdsAccount has no vault to read spec.keysRef")
	}
	data, err := vault.Get(ctx, *id.Spec.KeysRef)
	if err != nil {
		return pdsAccountCredential{}, fmt.Errorf("%w spec.keysRef %s/%s: %w", errVaultRead, id.Spec.KeysRef.Mount, id.Spec.KeysRef.Path, err)
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

func (a *Accounts) storedPdsSession(ctx context.Context, vault SecretStore, ref commonv1.SecretRef, pdsURL string, tlsConfig *tls.Config) (pdsclient.Session, bool) {
	if vault == nil {
		return pdsclient.Session{}, false
	}
	data, err := vault.Get(ctx, ref)
	if err != nil {
		return pdsclient.Session{}, false
	}
	access := firstString(data, "accessJwt", "accessToken", "access_token")
	if !oauthclient.AccessTokenLive(access) {
		return pdsclient.Session{}, false
	}
	tokens := pdsclient.TokenSourceFunc(func(ctx context.Context) (string, error) { return access, nil })
	pds, err := a.remotePDS(pdsURL, tokens, tlsConfig)
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

func savePdsSession(ctx context.Context, vault SecretStore, ref commonv1.SecretRef, pdsURL string, session pdsclient.Session) error {
	if vault == nil {
		return errors.New("marketidentity: no vault writes the pds account session")
	}
	return vault.Put(ctx, ref, map[string]any{
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

func (a *Accounts) tlsFor(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (*tls.Config, error) {
	if id.Spec.CARef == nil || id.Spec.CARef.IsZero() {
		return nil, nil
	}
	if a.tls == nil {
		return nil, errors.New("marketidentity: spec.caRef is set but no CA source is configured")
	}
	return a.tls(ctx, workspace, id)
}

func (a *Accounts) remotePDS(endpoint string, tokens pdsclient.TokenSource, tlsConfig *tls.Config) (pdsclient.PDS, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("marketidentity: the remote PDS has no URL")
	}
	if !strings.Contains(endpoint, "://") {
		return nil, fmt.Errorf("marketidentity: the remote PDS %q is not an absolute URL", endpoint)
	}
	cfg := pdsclient.RemoteConfig{BaseURL: endpoint, Tokens: tokens, Timeout: requestTimeout, HTTPClient: a.resolver.HTTPClient,
		TLS: a.resolver.TLS, DPoP: a.resolver.DPoP}
	if a.resolver.Timeout > 0 {
		cfg.Timeout = a.resolver.Timeout
	}
	switch {
	case tlsConfig != nil:
		cfg.HTTPClient, cfg.TLS = nil, tlsConfig
	case cfg.HTTPClient == nil:
		cfg.HTTPClient = a.http
	}
	return pdsclient.NewRemote(cfg)
}

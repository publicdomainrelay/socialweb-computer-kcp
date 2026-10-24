package atproto

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const signedOutMessage = "signed out through spec.signOutGeneration; sign in again"

func SignOutRequested(id *v1alpha1.MarketIdentity, status v1alpha1.MarketIdentityStatus) bool {
	return id.Spec.SignOutGeneration > status.SignedOutGeneration
}

func SignOutRefusal(mode v1alpha1.MarketIdentityMode) error {
	switch mode {
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
		return nil
	case v1alpha1.MarketIdentityModeOAuthSessionProxy:
		return errors.New("marketidentity: sign out is refused: mode oauthSessionProxy uses the web app's session, which the web app owns; sign out in the web app")
	}
	return fmt.Errorf("marketidentity: sign out is refused: mode %q holds no oauth session to sign out of", mode)
}

func (a *Accounts) Reconcile(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, status v1alpha1.MarketIdentityStatus) (Result, error) {
	if !SignOutRequested(id, status) {
		return a.reconcile(ctx, workspace, id, status)
	}
	if refusal := SignOutRefusal(id.Spec.Mode); refusal != nil {
		result, err := a.reconcile(ctx, workspace, id, status)
		message := strings.TrimPrefix(refusal.Error(), "marketidentity: ")
		if current := strings.TrimSpace(result.Status.Message); strings.Contains(current, message) {
			message = current
		} else if current != "" {
			message = current + "; " + message
		}
		result.Status.Message = message
		return result, err
	}
	next, err := a.signOut(ctx, workspace, id, status)
	if err != nil {
		return pending(status, id.Generation, "SignOutFailed", err.Error()), err
	}
	return a.reconcile(ctx, workspace, id, next)
}

func (a *Accounts) signOut(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, status v1alpha1.MarketIdentityStatus) (v1alpha1.MarketIdentityStatus, error) {
	key := qrKey(workspace, id.Name)
	if issue, ok := a.browser.Get(key); ok && !issue.spent {
		if _, _, err := a.statesFor(workspace).Take(ctx, issue.state); err != nil {
			a.log.Warn("the pending oauth callback state is not removed at sign out", "workspace", workspace, "identity", id.Name, "err", err.Error())
		}
	}
	a.browser.Delete(key)
	a.qr.Delete(key)
	ref := oauthSessionRef(id, &status)
	a.revoke(ctx, workspace, id, ref)
	a.held.forget(workspace, id.Name)
	if ref != nil {
		vault := a.vaultFor(workspace)
		if vault == nil {
			return status, errors.New("marketidentity: no vault deletes the oauth session at sign out")
		}
		if err := vault.Delete(ctx, *ref); err != nil && !errors.Is(err, ErrSecretNotFound) {
			return status, fmt.Errorf("marketidentity: deleting the oauth session at sign out: %w", err)
		}
	}
	generation := id.Generation
	status.SignedOutGeneration = id.Spec.SignOutGeneration
	status.SignedOutAt = time.Now().UTC().Format(time.RFC3339)
	status.Did, status.Handle, status.PdsURL = "", "", ""
	status.PendingAction = nil
	status.Message = signedOutMessage
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, false, v1alpha1.ReasonSignedOut, signedOutMessage)
	setReady(&status, generation, false, v1alpha1.ReasonSignedOut, signedOutMessage)
	a.log.Info("the identity is signed out", "workspace", workspace, "identity", id.Name, "mode", id.Spec.Mode,
		"signOutGeneration", id.Spec.SignOutGeneration)
	return status, nil
}

func (a *Accounts) revoke(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, ref *commonv1.SecretRef) {
	log := a.log.With("workspace", workspace, "identity", id.Name)
	session := a.held.session(workspace, id.Name)
	if session == nil && ref != nil {
		session = a.storedSession(ctx, workspace, id, *ref)
	}
	if session == nil {
		log.Info("no oauth session is held or stored; nothing to revoke at sign out")
		return
	}
	revocation, err := session.Revoke(ctx)
	switch {
	case errors.Is(err, oauthclient.ErrNoRevocationEndpoint):
		log.Info("the authorization server names no revocation endpoint; the oauth session is only forgotten", "did", session.DID())
	case err != nil:
		log.Warn("the oauth session is not revoked at the authorization server", "did", session.DID(), "endpoint", revocation.Endpoint,
			"revoked", revocation.Revoked, "err", err.Error())
	default:
		log.Info("the oauth session is revoked at the authorization server", "did", session.DID(), "endpoint", revocation.Endpoint,
			"revoked", revocation.Revoked)
	}
}

func (a *Accounts) storedSession(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, ref commonv1.SecretRef) *oauthclient.Session {
	vault := a.vaultFor(workspace)
	if vault == nil {
		return nil
	}
	stored, err := (&vaultSessionStore{vault: vault, ref: ref}).List(ctx)
	if err != nil || len(stored) == 0 {
		return nil
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return nil
	}
	data := stored[0]
	if clientID := a.pinnedClientID(id); clientID != "" {
		data.ClientID = clientID
	}
	session, err := oauthclient.New(data, oauthclient.Options{ClientID: a.sessionClientID(id), HTTPClient: client, Log: a.log})
	if err != nil {
		return nil
	}
	return session
}

func signedOut(status *v1alpha1.MarketIdentityStatus) bool {
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.ConditionSignedIn {
			return c.Reason == v1alpha1.ReasonSignedOut
		}
	}
	return false
}

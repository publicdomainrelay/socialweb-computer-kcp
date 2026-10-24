package atproto

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/identity"
)

type SessionsFunc func(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (vault.AtprotoSessions, error)

var ErrSignInAgain = errors.New("marketidentity: the owner must sign in to the web app again")

func (a *Accounts) sessionProxy(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64,
	status v1alpha1.MarketIdentityStatus) (Result, error) {
	did := strings.TrimSpace(id.Spec.Did)
	ref := id.Spec.SessionRef
	if did == "" || ref == nil || ref.Vault == nil {
		return failed(status, generation, "InvalidSpec",
			"mode oauthSessionProxy needs spec.did and a spec.sessionRef naming the vault, the atproto auth mount and session/<did>"), nil
	}
	session, err := a.proxiedSession(ctx, workspace, id)
	switch {
	case errors.Is(err, vault.ErrNoAtprotoSession):
		return a.signInAgain(status, generation, id, "NoSession",
			fmt.Sprintf("the web app holds no session for %s in %s; the owner must sign in to the web app again", did, ref.Mount)), ErrSignInAgain
	case errors.Is(err, vault.ErrAtprotoSessionInactive):
		return a.signInAgain(status, generation, id, "SessionInactive",
			fmt.Sprintf("the web app session of %s is not active (%v); the owner must sign in to the web app again", did, err)), ErrSignInAgain
	case err != nil:
		return pending(status, generation, "SessionProxyUnavailable", "reading the web app session of "+did+" through the vault: "+err.Error()), err
	case session.DID != did:
		return failed(status, generation, "SubjectMismatch", fmt.Sprintf("the web app session at %s/%s is for %s, not %s", ref.Mount, ref.Path, session.DID, did)), nil
	case session.Status != vault.AtprotoSessionActive:
		return a.signInAgain(status, generation, id, "SessionInactive",
			fmt.Sprintf("the web app session of %s is %s (%s); the owner must sign in to the web app again", did, session.Status, session.StatusReason)), ErrSignInAgain
	case strings.TrimSpace(session.PDS) == "":
		return pending(status, generation, "SessionProxyUnavailable", "the web app session of "+did+" names no PDS"), nil
	}
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	status.Did = did
	status.Handle = firstNonEmpty(session.Handle, id.Spec.Handle)
	status.PdsURL = strings.TrimRight(session.PDS, "/")
	status.SessionRef = ref
	status.Associations = nil
	status.PendingAction = nil
	status.Message = ""
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, true, "SignedIn",
		"the web app session in "+ref.Mount+" is active; the atproto auth plugin refreshes it and signs each request")
	setReady(&status, generation, true, "SignedIn", did)
	return Result{Status: status}, nil
}

func (a *Accounts) signInAgain(status v1alpha1.MarketIdentityStatus, generation int64, id *v1alpha1.MarketIdentity, reason, message string) Result {
	status.Phase = v1alpha1.MarketIdentityPhaseSessionExpired
	status.ObservedGeneration = generation
	status.SessionRef = id.Spec.SessionRef
	status.PendingAction = &v1alpha1.MarketIdentityPendingAction{Kind: v1alpha1.PendingActionSignInAgain, Handle: id.Spec.Handle}
	status.Message = message
	conds.Set(&status.Conditions, generation, v1alpha1.ConditionSignedIn, false, reason, message)
	setReady(&status, generation, false, reason, message)
	return Result{Status: status}
}

func (a *Accounts) proxiedSession(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (vault.AtprotoSession, error) {
	if a.sessions == nil {
		return vault.AtprotoSession{}, errors.New("marketidentity: this core resolves no vault atproto session handle")
	}
	sessions, err := a.sessions(ctx, workspace, id)
	if err != nil {
		return vault.AtprotoSession{}, err
	}
	return sessions.Session(ctx, id.Spec.SessionRef.Mount, id.Spec.SessionRef.Path)
}

func (a *Accounts) holdProxy(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, held *heldAccount) error {
	ref := id.Spec.SessionRef
	did, pdsURL := strings.TrimSpace(id.Status.Did), strings.TrimSpace(id.Status.PdsURL)
	if ref == nil || ref.Vault == nil || did == "" || pdsURL == "" || id.Status.Phase != v1alpha1.MarketIdentityPhaseReady {
		return fmt.Errorf("%w: the web app session of %s is not ready (%s)", ErrSignInAgain, firstNonEmpty(did, id.Spec.Did), id.Status.Message)
	}
	if a.sessions == nil {
		return errors.New("marketidentity: this core resolves no vault atproto session handle")
	}
	held.open.Lock()
	defer held.open.Unlock()
	if p := held.auth.Proxied(); p != nil && held.ref == *ref && held.auth.DID() == did && p.PdsURL == pdsURL {
		return nil
	}
	client, err := a.sessionHTTP(ctx, workspace, id)
	if err != nil {
		return err
	}
	mount, path := ref.Mount, ref.Path
	snapshot := id.DeepCopy()
	prover := xrpc.DPoPProverFunc(func(ctx context.Context, method, target, _, nonce string) (xrpc.DPoPProof, error) {
		sessions, err := a.sessions(ctx, workspace, snapshot)
		if err != nil {
			return xrpc.DPoPProof{}, err
		}
		proof, err := sessions.Proof(ctx, mount, path, method, target, nonce)
		if err != nil {
			return xrpc.DPoPProof{}, err
		}
		return xrpc.DPoPProof{AccessToken: proof.AccessToken, Proof: proof.DPoP}, nil
	})
	held.ref = *ref
	held.auth.HoldProxy(did, &identity.Proxy{PdsURL: pdsURL, HTTP: client, Prover: prover})
	return nil
}

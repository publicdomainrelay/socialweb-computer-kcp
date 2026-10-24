package atproto

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	ComponentKind = "Pds"

	ComponentName = "pds"

	ConditionReady = "Ready"

	ReasonRevoked = "Revoked"

	requestTimeout = 5 * time.Second

	minRefreshAfter = time.Second
)

type EndpointFunc func(workspace, kind, name string) (string, bool)

type VaultFunc func(workspace string) SecretStore

type TLSFunc func(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity) (*tls.Config, error)

type Config struct {
	Endpoints EndpointFunc

	Vault VaultFunc

	Resolver *pdsclient.Resolver

	HTTPClient *http.Client

	TLS TLSFunc

	Sessions SessionsFunc

	OAuth OAuthOptions

	Log *slog.Logger
}

type Result struct {
	Status v1alpha1.MarketIdentityStatus

	RequeueAfter time.Duration
}

type Accounts struct {
	endpoints EndpointFunc

	vault VaultFunc

	log *slog.Logger

	http *http.Client

	resolver *pdsclient.Resolver

	tls TLSFunc

	sessions SessionsFunc

	oauth OAuthOptions

	states oauthclient.StateStore

	qr *ttlcache.Cache[string, *qrTransfer]

	browser *ttlcache.Cache[string, browserIssue]

	held heldAccounts
}

func New(cfg Config) *Accounts {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = &pdsclient.Resolver{HTTPClient: client, Timeout: requestTimeout}
	}
	states := cfg.OAuth.States
	if states == nil {
		states = &oauthclient.MemoryStateStore{}
	}
	return &Accounts{endpoints: cfg.Endpoints, vault: cfg.Vault, log: log, http: client, resolver: resolver, tls: cfg.TLS, sessions: cfg.Sessions, oauth: cfg.OAuth,
		states: states, qr: ttlcache.New[string, *qrTransfer](ttlcache.Options{}), browser: ttlcache.New[string, browserIssue](ttlcache.Options{})}
}

func (a *Accounts) Reconcile(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, status v1alpha1.MarketIdentityStatus) (Result, error) {
	generation := id.Generation
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo && strings.TrimSpace(status.Did) != "" && status.ObservedGeneration >= generation &&
		status.Phase != "" && status.Phase != v1alpha1.MarketIdentityPhasePending {
		if status.Phase == v1alpha1.MarketIdentityPhaseReady || status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval {
			settleAssociation(&status, generation, id.Spec.Association, "the PDS reports "+status.Did)
		}
		return Result{Status: status}, nil
	}
	switch id.Spec.Mode {
	case v1alpha1.MarketIdentityModeLocalRepo:
		return a.localRepo(ctx, workspace, id, generation, status)
	case v1alpha1.MarketIdentityModePdsAccount:
		return a.pdsAccount(ctx, workspace, id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthBrowser:
		return a.oauthBrowser(ctx, workspace, id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthQr:
		return a.oauthQr(ctx, workspace, id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthDelegated:
		return a.oauthDelegated(ctx, workspace, id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthSessionProxy:
		return a.sessionProxy(ctx, workspace, id, generation, status)
	case "":
		return pending(status, generation, "UnknownMode", "the MarketIdentity has no mode; no DID is recorded"), nil
	default:
		return pending(status, generation, "UnknownMode", fmt.Sprintf("unknown mode %q; no DID is recorded", id.Spec.Mode)), nil
	}
}

func (a *Accounts) Forget(workspace, name string) {
	a.qr.Delete(qrKey(workspace, name))
	a.browser.Delete(qrKey(workspace, name))
	a.held.forget(workspace, name)
}

func (a *Accounts) localRepo(ctx context.Context, workspace string, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus) (Result, error) {
	endpoint := ""
	if a.endpoints != nil {
		if url, ok := a.endpoints(workspace, ComponentKind, ComponentName); ok {
			endpoint = strings.TrimSpace(url)
		}
	}
	if endpoint == "" {
		return pending(status, generation, "PdsNotReady", "waiting for the Pds/pds component endpoint in workspace "+workspace), nil
	}
	did, err := a.resolveDID(ctx, workspace, endpoint)
	if err != nil {
		return pending(status, generation, "PdsNotReady", "waiting for the PDS at "+endpoint+": "+err.Error()),
			fmt.Errorf("marketidentity: the PDS at %s: %w", endpoint, err)
	}
	composed := strings.TrimSpace(id.Spec.Did)
	if composed == "" {
		status.Did = ""
		return pending(status, generation, "AccountPending", "the PDS at "+endpoint+" ("+did+") is ready; waiting for the root to compose the account DID"), nil
	}
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	status.Did = composed
	if handle := strings.TrimSpace(id.Spec.Handle); handle != "" {
		status.Handle = handle
	}
	if pdsURL := strings.TrimSpace(id.Spec.PdsURL); pdsURL != "" {
		status.PdsURL = pdsURL
	}
	setAssociated(&status, generation, id.Spec.Association)
	settleAssociation(&status, generation, id.Spec.Association, "the PDS at "+endpoint+" reports "+did)
	return Result{Status: status}, nil
}

func settleAssociation(status *v1alpha1.MarketIdentityStatus, generation int64, mode v1alpha1.AssociationMode, ready string) {
	if mode != v1alpha1.AssociationSkip && !associatedTrue(status) {
		status.Phase = v1alpha1.MarketIdentityPhaseAwaitingApproval
		status.Message = "the PDS is ready; waiting for the operator's verified association claim"
		setReady(status, generation, false, "AwaitingAssociation", status.Message)
		return
	}
	status.Phase, status.Message = v1alpha1.MarketIdentityPhaseReady, ""
	setReady(status, generation, true, "Ready", ready)
}

func (a *Accounts) resolveDID(ctx context.Context, workspace, endpoint string) (string, error) {
	pds, err := a.localPDS(workspace, endpoint)
	if err != nil {
		return "", err
	}
	info, err := pds.Describe(ctx)
	if err != nil {
		return "", err
	}
	did := strings.TrimSpace(info.DID.String())
	if did == "" {
		return "", fmt.Errorf("the PDS reported no did")
	}
	return did, nil
}

func (a *Accounts) localPDS(workspace, endpoint string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	if a.resolver.Handlers != nil {
		return a.resolver.ResolveLocal(pdsclient.LocalRef{Workspace: workspace, Kind: ComponentKind, Name: ComponentName, Endpoint: endpoint})
	}
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, HTTPClient: a.resolver.HTTPClient, TLS: a.resolver.TLS,
		Tokens: a.resolver.Tokens, DPoP: a.resolver.DPoP, Timeout: opt.Positive(a.resolver.Timeout, requestTimeout)})
}

func (a *Accounts) vaultFor(workspace string) SecretStore {
	if a.vault == nil {
		return nil
	}
	return a.vault(workspace)
}

func pending(status v1alpha1.MarketIdentityStatus, generation int64, reason, message string) Result {
	status.Phase = v1alpha1.MarketIdentityPhasePending
	status.ObservedGeneration = generation
	status.Message = message
	setReady(&status, generation, false, reason, message)
	return Result{Status: status}
}

func failed(status v1alpha1.MarketIdentityStatus, generation int64, reason, message string) Result {
	status.Phase = v1alpha1.MarketIdentityPhaseFailed
	status.ObservedGeneration = generation
	status.Message = message
	setReady(&status, generation, false, reason, message)
	return Result{Status: status}
}

func setReady(status *v1alpha1.MarketIdentityStatus, generation int64, ready bool, reason, message string) {
	conds.Set(&status.Conditions, generation, ConditionReady, ready, reason, message)
}

func refreshAfter(token string) time.Duration {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ExpiresAt == 0 {
		return 0
	}
	return max(time.Until(time.Unix(claims.ExpiresAt, 0).Add(-oauthclient.AccessSkew)), minRefreshAfter)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

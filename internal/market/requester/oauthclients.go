package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	atprotooauth "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

const (
	OAuthRolePrefix = "oauth-"

	OAuthProtectsScope = "com.fedproxy.temp.oauth"

	OAuthExchangeTokenNSID = "com.fedproxy.temp.oauth.exchangeToken"

	OAuthApproveAuthorizationNSID = "com.fedproxy.temp.oauth.approveAuthorization"

	OAuthWIDFile = "/etc/socialweb-computer/oauth-wid.json"
)

type OAuthWIDDocument struct {
	Account string `json:"account"`

	PDS string `json:"pds"`

	Role string `json:"role"`

	Audience string `json:"audience"`

	Clients []v1alpha1.ComputeContractOAuthClient `json:"clients"`
}

func OAuthRoleName(vmName string) string {
	return OAuthRolePrefix + vmName
}

func OAuthAudience(account, role string) string {
	return account + "#role/" + role
}

func PDSServiceDID(pdsURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(pdsURL))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("requester: the PDS URL %q names no host", pdsURL)
	}
	did := "did:web:" + url.QueryEscape(u.Host)
	if _, err := syntax.ParseDID(did); err != nil {
		return "", fmt.Errorf("requester: the PDS URL %q gives no service DID: %w", pdsURL, err)
	}
	return did, nil
}

func CheckOAuthClients(clients []v1alpha1.ComputeContractOAuthClient) error {
	seen := map[string]bool{}
	for _, client := range clients {
		id := strings.TrimSpace(client.ClientID)
		u, err := url.Parse(id)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("oauthClients: clientId %q is not an https URL", client.ClientID)
		}
		if seen[id] {
			return fmt.Errorf("oauthClients: clientId %q is listed twice", id)
		}
		seen[id] = true
		if raw := strings.TrimSpace(client.RedirectURI); raw != "" {
			r, err := url.Parse(raw)
			if err != nil || r.Scheme != "https" || r.Host == "" {
				return fmt.Errorf("oauthClients: redirectUri %q of %s is not an https URL", raw, id)
			}
		}
		if len(client.Scopes) == 0 {
			return fmt.Errorf("oauthClients: %s names no scope; list the scopes the guest may approve (atproto and the repo: or rpc: scopes the app asks for)", id)
		}
		for _, scope := range client.Scopes {
			switch {
			case !atprotooauth.ValidScope(scope):
				return fmt.Errorf("oauthClients: %q of %s is not an atproto scope", scope, id)
			case strings.HasPrefix(scope, "transition:"):
				return fmt.Errorf("oauthClients: %s may not be granted to a workload (%s)", scope, id)
			case strings.HasPrefix(scope, "repo:") && (atprotooauth.ScopeSubject(scope) == atprotooauth.CollectionFedproxyRBAC || atprotooauth.ScopeSubject(scope) == "*"):
				return fmt.Errorf("oauthClients: %s would let the app grant further workloads (%s)", scope, id)
			}
		}
	}
	return nil
}

func (q *Requester) checkOAuthClients(contract v1alpha1.ComputeContract) error {
	if len(contract.Spec.OAuthClients) == 0 {
		return nil
	}
	if !q.cfg.LocalSigner || q.cfg.Spec.PdsRef == nil || q.cfg.Spec.PdsRef.Name == "" {
		return errors.New("spec.oauthClients needs the requester's account on the requester's own PDS (identity mode localRepo, for example run request --identity <name>); a guest approves OAuth only on a PDS of ours, never on bsky.social or another remote PDS")
	}
	if strings.TrimSpace(q.pdsURL) == "" {
		return errors.New("spec.oauthClients needs the requester's PDS URL, and the identity reports none yet")
	}
	return CheckOAuthClients(contract.Spec.OAuthClients)
}

func oauthClientsRecord(role, service, issuer, audience, subject string, clients []v1alpha1.ComputeContractOAuthClient) map[string]any {
	policies := map[string]any{}
	names := make([]string, 0, len(clients))
	for i, client := range clients {
		name := role
		if len(clients) > 1 {
			name = role + "-" + strconv.Itoa(i+1)
		}
		names = append(names, name)
		scopes := make([]any, 0, len(client.Scopes))
		for _, s := range client.Scopes {
			scopes = append(scopes, s)
		}
		redirect := map[string]any{"type": "string"}
		if r := strings.TrimSpace(client.RedirectURI); r != "" {
			redirect = map[string]any{"const": r}
		}
		policies[name] = map[string]any{
			"meta": map[string]any{"policy": name},
			"schemas": map[string]any{
				"/xrpc/" + OAuthExchangeTokenNSID: map[string]any{
					"type":       "object",
					"required":   []any{"capability"},
					"properties": map[string]any{"capability": map[string]any{"enum": []any{"create"}}},
				},
				"/xrpc/" + OAuthApproveAuthorizationNSID: map[string]any{
					"type":     "object",
					"required": []any{"capability", "body"},
					"properties": map[string]any{
						"capability": map[string]any{"enum": []any{"create"}},
						"body": map[string]any{
							"type":                 "object",
							"required":             []any{"client_id", "scope"},
							"additionalProperties": false,
							"properties": map[string]any{
								"client_id":    map[string]any{"const": strings.TrimSpace(client.ClientID)},
								"redirect_uri": redirect,
								"scope":        map[string]any{"type": "array", "items": map[string]any{"enum": scopes}},
							},
						},
					},
				},
			},
		}
	}
	policyNames := make([]any, 0, len(names))
	for _, n := range names {
		policyNames = append(policyNames, n)
	}
	return map[string]any{
		"$type":    FedproxyRBACNSID,
		"protects": map[string]any{role: map[string]any{"service": service, "scope": OAuthProtectsScope}},
		"roles": map[string]any{role: map[string]any{
			"role_name": role,
			"definition": map[string]any{
				"iss":      issuer,
				"aud":      audience,
				"sub":      subject,
				"policies": policyNames,
			},
		}},
		"policies":  policies,
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

func (r *contractRunner) prepareOAuthClients(ctx context.Context, state *v1alpha1.NodeClaimContract, winner *BidRecord) error {
	contract := r.snapshot()
	clients := contract.Spec.OAuthClients
	if winner == nil || len(clients) == 0 {
		return nil
	}
	q := r.q
	if err := q.checkOAuthClients(contract); err != nil {
		return err
	}
	vmName := state.VMName
	if vmName == "" {
		vmName = contract.Spec.VMName
	}
	role := OAuthRoleName(vmName)
	if _, err := syntax.ParseRecordKey(role); err != nil {
		return fmt.Errorf("requester: the OAuth role %q is not a record key: %w", role, err)
	}
	reader := market.NewHTTPReader(q.deps.Directory)
	reader.Client = q.deps.HTTP
	cfg, err := resolveBidConfig(ctx, reader, winner.Record)
	if err != nil {
		return err
	}
	subject, err := q.deps.Guests.Subject(winner.DID, q.callbackDID(), vmName)
	if err != nil {
		return err
	}
	service, err := PDSServiceDID(q.pdsURL)
	if err != nil {
		return err
	}
	account := q.repoDID
	audience := OAuthAudience(account, role)
	record := oauthClientsRecord(role, service, cfg.IssuerURI, audience, subject, clients)
	ref, err := q.repo.PutRecord(ctx, account, FedproxyRBACNSID, role, record)
	if err != nil {
		return fmt.Errorf("requester: writing the OAuth clients record %s: %w", role, err)
	}
	state.OAuthClients = &v1alpha1.ContractOAuthClients{
		Role:      role,
		Audience:  audience,
		Service:   service,
		Record:    &commonv1.StrongRef{URI: ref.URI, CID: ref.CID},
		Clients:   append([]v1alpha1.ComputeContractOAuthClient(nil), clients...),
		WrittenAt: q.nowISO(),
	}
	r.log().Info("requester_oauth_clients_written", "role", role, "uri", ref.URI, "clients", len(clients))
	if contract.Spec.SecretsRef == nil {
		return nil
	}
	doc, err := json.Marshal(OAuthWIDDocument{Account: account, PDS: strings.TrimRight(q.pdsURL, "/"), Role: role, Audience: audience, Clients: clients})
	if err != nil {
		return err
	}
	return r.putBundleEntry(ctx, contract, SecretEntry{Path: OAuthWIDFile, Value: string(doc) + "\n", Mode: "0600"})
}

func (r *contractRunner) deleteOAuthClients(ctx context.Context) {
	q := r.q
	if q.repo == nil {
		return
	}
	role := ""
	if state := r.stateCopy(); state != nil && state.OAuthClients != nil {
		role = state.OAuthClients.Role
	}
	contract := r.snapshot()
	if role == "" && len(contract.Spec.OAuthClients) > 0 {
		vmName := contract.Spec.VMName
		if state := r.stateCopy(); state != nil && state.VMName != "" {
			vmName = state.VMName
		}
		if vmName != "" {
			role = OAuthRoleName(vmName)
		}
	}
	if role == "" {
		return
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := q.repo.DeleteRecord(deleteCtx, q.repoDID, FedproxyRBACNSID, role); err != nil && !rbacGone(err) {
		r.log().Warn("requester_oauth_clients_delete_failed", "role", role, "err", err)
		return
	}
	r.log().Info("requester_oauth_clients_deleted", "role", role)
}

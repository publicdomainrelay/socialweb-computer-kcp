package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	SecretsNSID = "com.publicdomainrelay.temp.compute.secrets"

	SecretsRoute = "/xrpc/" + SecretsNSID + ".getSecrets"

	SecretsAPIAudience = "ATProto"

	FedproxyRBACNSID = "com.fedproxy.rbac"
)

type SecretEntry struct {
	Path string `json:"path"`

	Value string `json:"value"`
}

type secretsState struct {
	mu sync.Mutex

	name string

	log *slog.Logger

	entries []SecretEntry

	issuer string

	audience string

	subject string

	verifier TokenVerifier

	fetchedAt string

	fetched chan struct{}

	once sync.Once
}

func newSecretsState(name string, log *slog.Logger) *secretsState {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &secretsState{name: name, log: log, fetched: make(chan struct{})}
}

func (s *secretsState) install(issuer, audience, subject string, verifier TokenVerifier) {
	s.mu.Lock()
	s.issuer, s.audience, s.subject, s.verifier = issuer, audience, subject, verifier
	s.mu.Unlock()
}

func (s *secretsState) addEntries(entries ...SecretEntry) {
	s.mu.Lock()
	for _, entry := range entries {
		replaced := false
		for i := range s.entries {
			if s.entries[i].Path == entry.Path {
				s.entries[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			s.entries = append(s.entries, entry)
		}
	}
	s.mu.Unlock()
}

func (s *secretsState) fetchedAtValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetchedAt
}

func (s *secretsState) markFetched(now string) bool {
	s.mu.Lock()
	if s.fetchedAt != "" {
		s.mu.Unlock()
		return false
	}
	s.fetchedAt = now
	s.mu.Unlock()
	s.once.Do(func() { close(s.fetched) })
	return true
}

func (s *secretsState) fetch(ctx context.Context, authorization string) ([]SecretEntry, error) {
	s.mu.Lock()
	issuer, audience, subject, verifier := s.issuer, s.audience, s.subject, s.verifier
	entries := append([]SecretEntry(nil), s.entries...)
	already := s.fetchedAt
	s.mu.Unlock()
	if verifier == nil || issuer == "" || audience == "" || subject == "" {
		s.log.Warn("the secrets server is not ready", "contract", s.name)
		return nil, &RequestError{Status: http.StatusServiceUnavailable, Code: "NotReady"}
	}
	token, ok := strings.CutPrefix(strings.TrimSpace(authorization), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		s.log.Warn("the guest's secrets request carries no bearer token", "contract", s.name, "err", "requester: no bearer token")
		return nil, &RequestError{Status: http.StatusUnauthorized, Code: "AuthenticationRequired"}
	}
	claims, err := verifier.VerifyAudience(ctx, strings.TrimSpace(token), audience)
	if err != nil {
		s.log.Warn("the guest's secrets token is not accepted", "contract", s.name, "issuer", issuer, "err", err.Error())
		return nil, &RequestError{Status: http.StatusUnauthorized, Code: "InvalidToken"}
	}
	if got, _ := claims[cloudinit.NodeJWTUserClaim].(string); got != subject {
		s.log.Warn("the guest's secrets token names another subject", "contract", s.name, "subject", subject)
		return nil, &RequestError{Status: http.StatusUnauthorized, Code: "InvalidToken"}
	}
	if already == "" {
		s.markFetched(time.Now().UTC().Format(time.RFC3339Nano))
	}
	s.log.Info("the guest fetched its secrets", "contract", s.name, "entries", len(entries))
	return entries, nil
}

func (q *Requester) Secrets(ctx context.Context, contract, authorization string) ([]SecretEntry, error) {
	q.secretsMu.Lock()
	state := q.secrets[contract]
	q.secretsMu.Unlock()
	if state == nil {
		return nil, &RequestError{Status: http.StatusNotFound, Code: "NotFound"}
	}
	return state.fetch(ctx, authorization)
}

func (q *Requester) registerSecrets(name string, state *secretsState) {
	q.secretsMu.Lock()
	q.secrets[name] = state
	q.secretsMu.Unlock()
}

func (q *Requester) unregisterSecrets(name string) {
	q.secretsMu.Lock()
	delete(q.secrets, name)
	q.secretsMu.Unlock()
}

func (q *Requester) readSecretsBundle(ctx context.Context, ref commonv1.SecretRef) ([]SecretEntry, error) {
	if q.deps.Vault == nil {
		return nil, errors.New("requester: the contract names a secrets bundle and the component names no OpenBaoServer")
	}
	kv, err := q.deps.Vault(ctx, ref)
	if err != nil {
		return nil, err
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil {
		return nil, fmt.Errorf("requester: reading the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	text, _ := data["bundle"].(string)
	if text == "" {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s carries no bundle", ref.Mount, ref.Path)
	}
	entries := []SecretEntry{}
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	for i := range entries {
		if entries[i].Path == "" || !strings.HasPrefix(entries[i].Path, "/") {
			return nil, fmt.Errorf("requester: the secrets bundle names the path %q", entries[i].Path)
		}
	}
	return entries, nil
}

func (q *Requester) ingressExposureURL(ctx context.Context) (string, error) {
	name := "svc-" + strings.ToLower(KindRequester) + "-" + strings.TrimSpace(q.cfg.Name) + "-ingress"
	if raw := q.deps.Inputs.AwaitAddress(ctx, name, DefaultExposureWait, nil); raw != "" {
		return raw, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("requester: the %s exposure reports no url", name)
}

func (q *Requester) issuerVerifier(issuer string) TokenVerifier {
	issuer = strings.TrimRight(issuer, "/")
	client := q.deps.HTTP
	return q.deps.Guests.Verifier(issuer, func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/keys", nil)
		if err != nil {
			return nil, err
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			return nil, fmt.Errorf("requester: the bidder OIDC keys answered HTTP %d", res.StatusCode)
		}
		return io.ReadAll(io.LimitReader(res.Body, 1<<20))
	})
}

func secretsAudience(requesterDID string) string {
	return "api://" + SecretsAPIAudience + "?actx=" + requesterDID
}

func secretsRouteOf(contract string) string {
	return SecretsRoute + "/" + contract
}

func secretsRBACKey(contract string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '~', r == ':', r == '-':
			return r
		default:
			return '-'
		}
	}, "node."+contract)
	if len(safe) > 512 {
		return safe[:512]
	}
	return safe
}

type bidConfig struct {
	IssuerURI string

	Actx string
}

func resolveBidConfig(ctx context.Context, reader market.RecordResolver, record map[string]any) (bidConfig, error) {
	raw, _ := record["config"].(map[string]any)
	uri, _ := raw["uri"].(string)
	cid, _ := raw["cid"].(string)
	if uri == "" {
		return bidConfig{}, errors.New("the winning bid carries no bid config reference")
	}
	resolved, err := reader.Resolve(ctx, market.RecordRef{URI: uri, CID: cid})
	if err != nil {
		return bidConfig{}, fmt.Errorf("reading the winning bid's config record: %w", err)
	}
	issuer, _ := resolved.Value["issuer_uri"].(string)
	actx, _ := resolved.Value["actx"].(string)
	if issuer == "" {
		return bidConfig{}, errors.New("the winning bid's config record carries no issuer_uri")
	}
	return bidConfig{IssuerURI: issuer, Actx: actx}, nil
}

func (q *Requester) nodeGrant(bidderDID, requesterDID, role, cluster string, cfg bidConfig) (*v1alpha1.NodeClaimGrant, error) {
	subject, err := q.deps.Guests.Subject(bidderDID, requesterDID, role)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.NodeClaimGrant{
		Issuer:   cfg.IssuerURI,
		Subject:  subject,
		Audience: cloudinit.NodeAudience(cluster, role),
		Role:     role,
	}, nil
}

func nodeClaimDocument(cluster, node, issuer string) map[string]any {
	return map[string]any{
		"node":     node,
		"cluster":  cluster,
		"jwtMount": cloudinit.NodeJWTMount(issuer),
		"role":     node,
		"audience": cloudinit.NodeAudience(cluster, node),
		"kvMount":  cloudinit.NodeKVMount,
		"kvPath":   node,
	}
}

func nodeRBACRecord(role, issuer, subject, secretsAud, nodeAud string, nodeJoin bool) map[string]any {
	roles := map[string]any{
		role: map[string]any{
			"role_name": role,
			"definition": map[string]any{
				"iss":      issuer,
				"aud":      secretsAud,
				"sub":      subject,
				"policies": []string{},
			},
		},
	}
	if nodeJoin {
		nodeRole := role + "-node"
		roles[nodeRole] = map[string]any{
			"role_name": nodeRole,
			"definition": map[string]any{
				"iss":      issuer,
				"aud":      nodeAud,
				"sub":      subject,
				"policies": []string{},
			},
		}
	}
	return map[string]any{
		"$type":     FedproxyRBACNSID,
		"roles":     roles,
		"policies":  map[string]any{},
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	}
}

func meshdPinOf(meshd *v1alpha1.ComputeContractMeshd) *cloudinit.MeshdPin {
	if meshd == nil || meshd.DownloadBase == "" {
		return nil
	}
	out := &cloudinit.MeshdPin{DownloadBase: meshd.DownloadBase, Sha256: map[string]string{}}
	for _, pin := range meshd.Pins {
		out.Sha256[pin.Arch] = pin.Sha256
	}
	return out
}

func nodeAgentPinFromJoin(join *v1alpha1.ComputeContractNodeJoin) *cloudinit.NodeAgentPin {
	if join == nil || join.NodeAgent == nil || join.NodeAgent.DownloadBase == "" {
		return nil
	}
	out := &cloudinit.NodeAgentPin{DownloadBase: join.NodeAgent.DownloadBase, Sha256: map[string]string{}}
	for _, pin := range join.NodeAgent.Pins {
		out.Sha256[pin.Arch] = pin.Sha256
	}
	return out
}

func contractAcceptPath(contract v1alpha1.ComputeContract) string {
	if contract.Spec.NodeJoin != nil {
		return cloudinit.NodeSecretsDir + "/accept.json"
	}
	return "/root/secrets/publicdomainrelay.com/market/accept.json"
}

func (r *contractRunner) ensureSecrets(ctx context.Context, contract v1alpha1.ComputeContract) error {
	if contract.Spec.SecretsRef == nil || r.secrets != nil {
		return nil
	}
	entries, err := r.q.readSecretsBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return err
	}
	url, err := r.q.ingressExposureURL(ctx)
	if err != nil {
		return err
	}
	state := newSecretsState(r.name, r.q.log)
	state.addEntries(entries...)
	r.q.registerSecrets(r.name, state)
	r.mu.Lock()
	r.secrets = state
	r.secretsURL = url
	r.mu.Unlock()
	if grant := contract.Status.Grant; grant != nil && contract.Spec.NodeJoin != nil {
		audience := secretsAudience(r.q.callbackDID())
		state.install(grant.Issuer, audience, grant.Subject, r.q.issuerVerifier(grant.Issuer))
		doc, err := json.Marshal(nodeClaimDocument(contract.Spec.NodeJoin.ClusterRef.Name, grant.Role, grant.Issuer))
		if err != nil {
			return err
		}
		state.addEntries(SecretEntry{Path: cloudinit.NodeClaimDocPath, Value: string(doc) + "\n"})
		record := nodeRBACRecord(grant.Role, grant.Issuer, grant.Subject, audience, grant.Audience, true)
		key := secretsRBACKey(r.name)
		if _, err := r.q.repo.PutRecord(ctx, r.q.repoDID, FedproxyRBACNSID, key, record); err != nil {
			return fmt.Errorf("requester: rewriting the contract's RBAC record: %w", err)
		}
		r.mu.Lock()
		r.rbacRKey = key
		r.mu.Unlock()
	}
	return nil
}

func (r *contractRunner) contractModules(userData string, contract v1alpha1.ComputeContract) (string, error) {
	modules := []any{}
	values := cloudinit.Context{}
	if contract.Spec.NodeJoin != nil {
		opts := cloudinit.K3sJoinOptions{Meshd: meshdPinOf(contract.Spec.NodeJoin.Meshd),
			NodeAgent: nodeAgentPinFromJoin(contract.Spec.NodeJoin)}
		modules = append(modules, "k3s", cloudinit.K3sJoinModule(opts))
	}
	if len(contract.Spec.Tunnels) > 0 {
		opts, err := tunnelsOptions(contract)
		if err != nil {
			return "", fmt.Errorf("the contract's tunnels: %w", err)
		}
		modules = append(modules, cloudinit.TunnelsModule(opts))
	}
	if r.secrets != nil {
		modules = append(modules, "secrets")
		values.SecretsURL = r.secretsURLValue()
		values.SecretsRoute = secretsRouteOf(r.name)
		values.SecretsAud = secretsAudience(r.q.callbackDID())
		values.SecretsAcceptPath = contractAcceptPath(contract)
	}
	if len(modules) == 0 {
		return userData, nil
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: modules, Ctx: values})
}

func (r *contractRunner) prepareWorkloadGrant(ctx context.Context, state *v1alpha1.NodeClaimContract, winner *BidRecord) error {
	if winner == nil || (r.nodeJoin == nil && r.secrets == nil) {
		return nil
	}
	reader := market.NewHTTPReader(r.q.deps.Directory)
	reader.Client = r.q.deps.HTTP
	cfg, err := resolveBidConfig(ctx, reader, winner.Record)
	if err != nil {
		return err
	}
	vmName := state.VMName
	if vmName == "" {
		vmName = r.snapshot().Spec.VMName
	}
	subject := ""
	if r.nodeJoin != nil {
		grant, err := r.q.nodeGrant(winner.DID, r.q.callbackDID(), vmName, r.nodeJoin.ClusterRef.Name, cfg)
		if err != nil {
			return err
		}
		r.mu.Lock()
		r.grant = grant
		r.mu.Unlock()
		subject = grant.Subject
	} else {
		subject, err = r.q.deps.Guests.Subject(winner.DID, r.q.callbackDID(), vmName)
		if err != nil {
			return err
		}
	}
	if r.secrets == nil {
		return nil
	}
	audience := secretsAudience(r.q.callbackDID())
	r.secrets.install(cfg.IssuerURI, audience, subject, r.q.issuerVerifier(cfg.IssuerURI))
	if r.nodeJoin != nil {
		doc, err := json.Marshal(nodeClaimDocument(r.nodeJoin.ClusterRef.Name, vmName, cfg.IssuerURI))
		if err != nil {
			return err
		}
		r.secrets.addEntries(SecretEntry{Path: cloudinit.NodeClaimDocPath, Value: string(doc) + "\n"})
	}
	nodeAud := ""
	if r.nodeJoin != nil {
		nodeAud = cloudinit.NodeAudience(r.nodeJoin.ClusterRef.Name, vmName)
	}
	record := nodeRBACRecord(vmName, cfg.IssuerURI, subject, audience, nodeAud, r.nodeJoin != nil)
	key := secretsRBACKey(r.name)
	if _, err := r.q.repo.PutRecord(ctx, r.q.repoDID, FedproxyRBACNSID, key, record); err != nil {
		return fmt.Errorf("requester: writing the contract's RBAC record: %w", err)
	}
	r.mu.Lock()
	r.rbacRKey = key
	r.mu.Unlock()
	return nil
}

func (r *contractRunner) cleanup() {
	r.mu.Lock()
	secrets := r.secrets
	r.mu.Unlock()
	if secrets != nil {
		r.q.unregisterSecrets(r.name)
	}
}

func (r *contractRunner) deleteRBAC(ctx context.Context) {
	r.mu.Lock()
	key := r.rbacRKey
	r.mu.Unlock()
	if key == "" || r.q.repo == nil {
		return
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := r.q.repo.DeleteRecord(deleteCtx, r.q.repoDID, FedproxyRBACNSID, key); err != nil && !errors.Is(err, market.ErrNotFound) && !strings.Contains(err.Error(), "not found") {
		r.log().Warn("requester_rbac_delete_failed", "rkey", key, "err", err)
	}
	r.mu.Lock()
	r.rbacRKey = ""
	r.mu.Unlock()
}

func (r *contractRunner) secretsFetched() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secretsFetchedAt
}

func (r *contractRunner) setSecretsFetched(at string) {
	if at == "" {
		return
	}
	r.mu.Lock()
	r.secretsFetchedAt = at
	r.mu.Unlock()
}

func (r *contractRunner) secretsURLValue() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.secretsURL
}

func (r *contractRunner) grantValue() *v1alpha1.NodeClaimGrant {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.grant == nil {
		return nil
	}
	grant := *r.grant
	return &grant
}

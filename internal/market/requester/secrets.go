package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
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

	Mode string `json:"mode,omitempty"`
}

var secretModeRE = regexp.MustCompile(`^0?[0-6]?[0-7]?[0-7]?$`)

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

	unminted map[string]bool
}

func (s *secretsState) withholdUnminted(paths ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, path := range paths {
		if s.unminted == nil {
			s.unminted = map[string]bool{}
		}
		s.unminted[path] = true
	}
}

func (s *secretsState) minted(path string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.unminted[path] {
		return false
	}
	delete(s.unminted, path)
	return true
}

func (s *secretsState) servable(entries []SecretEntry) []SecretEntry {
	if s == nil {
		return entries
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.servableLocked(entries)
}

func (s *secretsState) servableLocked(entries []SecretEntry) []SecretEntry {
	if len(s.unminted) == 0 {
		return entries
	}
	out := make([]SecretEntry, 0, len(entries))
	for _, entry := range entries {
		if s.unminted[entry.Path] {
			continue
		}
		out = append(out, entry)
	}
	return out
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
	s.mu.Lock()
	entries := s.servableLocked(append([]SecretEntry(nil), s.entries...))
	s.mu.Unlock()
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
		return nil, retryable(err)
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil {
		return nil, retryable(fmt.Errorf("requester: reading the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err))
	}
	text, _ := data["bundle"].(string)
	if text == "" {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s carries no bundle", ref.Mount, ref.Path)
	}
	entries, err := ParseSecretsBundle(text)
	if err != nil {
		return nil, fmt.Errorf("requester: the secrets bundle %s/%s: %w", ref.Mount, ref.Path, err)
	}
	return entries, nil
}

func ParseSecretsBundle(text string) ([]SecretEntry, error) {
	entries := []SecretEntry{}
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, errors.New("the bundle is not a JSON array of {path, value, mode}")
	}
	for i := range entries {
		if entries[i].Path == "" || !strings.HasPrefix(entries[i].Path, "/") {
			return nil, fmt.Errorf("the bundle names the path %q", entries[i].Path)
		}
		if err := checkSecretMode(entries[i].Mode); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
	}
	return entries, nil
}

func checkSecretMode(raw string) error {
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseUint(raw, 8, 32)
	if err != nil || !secretModeRE.MatchString(raw) || value&^0o600 != 0 {
		return fmt.Errorf("mode %q is not an octal file mode of at most 0600", raw)
	}
	return nil
}

func (q *Requester) ingressExposureURL(ctx context.Context) (string, error) {
	name := "svc-" + strings.ToLower(KindRequester) + "-" + strings.TrimSpace(q.cfg.Name) + "-ingress"
	if raw := q.deps.Inputs.AwaitAddress(ctx, name, DefaultExposureWait, nil); raw != "" {
		return raw, nil
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return "", retryable(fmt.Errorf("requester: the %s exposure reports no url", name))
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

var errSecretsNeedIngress = errors.New("secrets need the requester ingress (getSecrets) or secretsFallback: Sealed")

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
	inbound, sealed := r.q.cfg.InboundXRPC(), r.q.cfg.SealedSecrets()
	if !inbound && !sealed {
		return errSecretsNeedIngress
	}
	entries, err := r.q.readSecretsBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return err
	}
	state := newSecretsState(r.name, r.q.log)
	state.addEntries(entries...)
	if envFile, err := agentEnvFile(contract); err == nil && envFile != "" && !agentPeerReady(entries, envFile) {
		state.withholdUnminted(envFile, cloudinit.AgentIrohKeyFile)
	}
	url := ""
	if inbound {
		if url, err = r.q.ingressExposureURL(ctx); err != nil {
			return err
		}
		r.q.registerSecrets(r.name, state)
	}
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
		key := r.rbacKeyOf(contract.Status.Contract)
		if key == "" {
			return nil
		}
		if err := r.putRBAC(ctx, key, record); err != nil {
			return fmt.Errorf("requester: rewriting the contract's RBAC record: %w", err)
		}
	}
	return nil
}

func (r *contractRunner) rbacKeyOf(state *v1alpha1.NodeClaimContract) string {
	switch {
	case state == nil:
		return ""
	case state.Accept != nil && state.Accept.URI != "":
		return market.RBACRKeyOfAccept(state.Accept.URI)
	case state.RFP != nil && state.RFP.URI != "" && state.Bid != nil && state.Bid.URI != "":
		return market.RBACRKey(acceptRKeyFor(r.q.repoDID, r.name, state.RFP.URI, state.RFP.CID, state.Bid.URI, state.Bid.CID))
	}
	return ""
}

func acceptRKeyFor(repoDID, contract, rfpURI, rfpCID, bidURI, bidCID string) string {
	return sessionRKey("accept", repoDID, contract, rfpURI, rfpCID, bidURI, bidCID)
}

func (r *contractRunner) putRBAC(ctx context.Context, key string, record map[string]any) error {
	if _, err := r.q.repo.PutRecord(ctx, r.q.repoDID, FedproxyRBACNSID, key, record); err != nil {
		return err
	}
	r.mu.Lock()
	r.rbacRKey = key
	r.mu.Unlock()
	r.deleteLegacyRBAC(ctx)
	return nil
}

func (r *contractRunner) deleteLegacyRBAC(ctx context.Context) {
	key := secretsRBACKey(r.name)
	if _, err := r.q.repo.GetRecord(ctx, r.q.repoDID, FedproxyRBACNSID, key); err != nil {
		if !rbacGone(err) {
			r.log().Warn("requester_rbac_legacy_unread", "rkey", key, "err", err)
		}
		return
	}
	if err := r.q.repo.DeleteRecord(ctx, r.q.repoDID, FedproxyRBACNSID, key); err != nil && !rbacGone(err) {
		r.log().Warn("requester_rbac_delete_failed", "rkey", key, "err", err)
	}
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
	envFile, err := agentEnvFile(contract)
	if err != nil {
		return "", fmt.Errorf("the contract's agent: %w", err)
	}
	if envFile != "" {
		modules = append(modules, cloudinit.AgentModule(cloudinit.AgentOptions{EnvFile: envFile}))
	}
	if a := contract.Spec.Association; a != nil && a.FromGuest == (strings.TrimSpace(a.Subject) != "") {
		return "", errors.New("spec.association takes exactly one of a subject or fromGuest")
	}
	if fromGuest(contract) {
		if contract.Spec.SecretsRef == nil {
			return "", errors.New("spec.association.fromGuest needs spec.secretsRef: the association ref reaches the guest in the secrets bundle")
		}
		refFile, err := associationRefFile(contract)
		if err != nil {
			return "", err
		}
		modules = append(modules, cloudinit.AssociationModule(refFile))
	}
	if len(contract.Spec.GuestTools) > 0 {
		modules = append(modules, cloudinit.GuestToolsModule(contract.Spec.GuestTools))
	}
	if r.secrets != nil {
		inbound := r.q.cfg.InboundXRPC()
		if inbound {
			modules = append(modules, "secrets")
			values.SecretsURL = r.secretsURLValue()
			values.SecretsRoute = secretsRouteOf(r.name)
			values.SecretsAud = secretsAudience(r.q.callbackDID())
			values.SecretsAcceptPath = contractAcceptPath(contract)
		}
		if r.q.cfg.SealedSecrets() {
			modules = append(modules, cloudinit.GuestSecretsModule(inbound))
		}
	}
	if len(modules) == 0 {
		return userData, nil
	}
	return cloudinit.BuildUserData(cloudinit.BuildOptions{Base: userData, Modules: modules, Ctx: values})
}

func (r *contractRunner) prepareWorkloadGrant(ctx context.Context, state *v1alpha1.NodeClaimContract, winner *BidRecord, acceptRKey string) error {
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
	if err := r.putRBAC(ctx, market.RBACRKey(acceptRKey), record); err != nil {
		return fmt.Errorf("requester: writing the contract's RBAC record: %w", err)
	}
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
	held := r.rbacRKey
	r.mu.Unlock()
	if r.q.repo == nil || (held == "" && r.snapshot().Spec.SecretsRef == nil) {
		return
	}
	deleteCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys := []string{held}
	if key := r.rbacKeyOf(r.stateCopy()); key != held {
		keys = append(keys, key)
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := r.q.repo.DeleteRecord(deleteCtx, r.q.repoDID, FedproxyRBACNSID, key); err != nil && !rbacGone(err) {
			r.log().Warn("requester_rbac_delete_failed", "rkey", key, "err", err)
		}
	}
	r.deleteLegacyRBAC(deleteCtx)
	r.mu.Lock()
	r.rbacRKey = ""
	r.mu.Unlock()
}

func rbacGone(err error) bool {
	return errors.Is(err, market.ErrNotFound) || strings.Contains(err.Error(), "not found")
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

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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

const (
	SecretsNSID = "com.publicdomainrelay.temp.compute.secrets"

	SecretsRoute = "/xrpc/" + SecretsNSID + ".getSecrets"

	SecretsAPIAudience = "ATProto"

	FedproxyRBACNSID = "com.fedproxy.rbac"

	DefaultSecretsFetchTimeout = 120 * time.Second

	DefaultExposureWait = 60 * time.Second
)

type secretEntry struct {
	Path string `json:"path"`

	Value string `json:"value"`
}

type secretsState struct {
	mu sync.Mutex

	name string

	log *slog.Logger

	entries []secretEntry

	issuer string

	audience string

	subject string

	verifier *guestidentity.Verifier

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

func (s *secretsState) install(issuer, audience, subject string, verifier *guestidentity.Verifier) {
	s.mu.Lock()
	s.issuer, s.audience, s.subject, s.verifier = issuer, audience, subject, verifier
	s.mu.Unlock()
}

func (s *secretsState) addEntries(entries ...secretEntry) {
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

func (s *secretsState) serve(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	issuer, audience, subject, verifier := s.issuer, s.audience, s.subject, s.verifier
	entries := append([]secretEntry(nil), s.entries...)
	already := s.fetchedAt
	s.mu.Unlock()
	if verifier == nil || issuer == "" || audience == "" || subject == "" {
		s.log.Warn("the secrets server is not ready", "contract", s.name)
		http.Error(w, `{"error":"NotReady"}`, http.StatusServiceUnavailable)
		return
	}
	token, err := bearerToken(r)
	if err != nil {
		s.log.Warn("the guest's secrets request carries no bearer token", "contract", s.name, "err", err.Error())
		http.Error(w, `{"error":"AuthenticationRequired"}`, http.StatusUnauthorized)
		return
	}
	claims, err := verifier.VerifyAudience(ctx, token, audience)
	if err != nil {
		s.log.Warn("the guest's secrets token is not accepted", "contract", s.name, "issuer", issuer, "err", err.Error())
		http.Error(w, `{"error":"InvalidToken"}`, http.StatusUnauthorized)
		return
	}
	got, _ := claims[cloudinit.NodeJWTUserClaim].(string)
	if got != subject {
		s.log.Warn("the guest's secrets token names another subject", "contract", s.name, "subject", subject)
		http.Error(w, `{"error":"InvalidToken"}`, http.StatusUnauthorized)
		return
	}
	if already == "" {
		s.markFetched(time.Now().UTC().Format(time.RFC3339Nano))
	}
	s.log.Info("the guest fetched its secrets", "contract", s.name, "entries", len(entries))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func bearerToken(r *http.Request) (string, error) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	token, ok := strings.CutPrefix(raw, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return "", errors.New("requester: no bearer token")
	}
	return strings.TrimSpace(token), nil
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

func newIssuerVerifier(issuer string, client *http.Client) *guestidentity.Verifier {
	issuer = strings.TrimRight(issuer, "/")
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	return guestidentity.NewVerifier(func(ctx context.Context) ([]byte, error) {
		target := issuer + "/.well-known/keys"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
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
	}, func() string { return issuer })
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

func nodeGrant(bidderDID, requesterDID, role, cluster string, cfg bidConfig) (*v1alpha1.NodeClaimGrant, error) {
	subject, err := guestidentity.SubjectFor(bidderDID, requesterDID, role)
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

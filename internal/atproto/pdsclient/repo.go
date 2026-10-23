package pdsclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const (
	DefaultRepoWorkspace = "root"

	defaultRepoTimeout = 30 * time.Second

	dpopProofLifetime = 30 * time.Second
)

type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (map[string]any, error)
}

type SecretResolverFunc func(ctx context.Context, ref string) (map[string]any, error)

func (f SecretResolverFunc) Resolve(ctx context.Context, ref string) (map[string]any, error) {
	if f == nil {
		return nil, fmt.Errorf("%w: nil secret resolver", ErrInvalidConfig)
	}
	return f(ctx, ref)
}

type RemotePDSSource func(ctx context.Context, workspace, name string) (*pdsv1alpha1.RemotePDS, error)

type CABundleFunc func(ref string) ([]byte, error)

type RepoBinding struct {
	Workspace string

	Kind string

	Name string

	Endpoint string

	DID string

	RemotePDS *pdsv1alpha1.RemotePDS

	AuthRef string

	SessionRef string
}

func (b RepoBinding) normalized() RepoBinding {
	b.Workspace = strings.TrimSpace(b.Workspace)
	if b.Workspace == "" {
		b.Workspace = DefaultRepoWorkspace
	}
	b.Kind = strings.TrimSpace(b.Kind)
	b.Name = strings.TrimSpace(b.Name)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.DID = strings.TrimSpace(b.DID)
	b.AuthRef = strings.TrimSpace(b.AuthRef)
	b.SessionRef = strings.TrimSpace(b.SessionRef)
	return b
}

func (b RepoBinding) remote() bool {
	if b.RemotePDS != nil {
		return true
	}
	switch strings.ToLower(b.Kind) {
	case "remotepds", "remote-pds", "remote":
		return true
	case "url", "operator", "operatorrepo", "operator-repo":
		return true
	}
	return !localKind(b.Kind) && strings.Contains(b.Endpoint, "://")
}

func localKind(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "pds", "local", "localpds", "local-pds":
		return true
	}
	return false
}

type ResolvedRepo struct {
	Binding RepoBinding

	DID syntax.DID

	PDS PDS

	Tokens TokenSource

	DPoP DPoPSource
}

type RepoResolver struct {
	Handlers *component.HandlerRegistry

	HTTPClient *http.Client

	TLS *tls.Config

	Timeout time.Duration

	Secrets SecretResolver

	RemotePDS RemotePDSSource

	CABundle CABundleFunc

	Tokens TokenSource

	DPoP DPoPSource
}

func (r *RepoResolver) Resolve(ctx context.Context, binding RepoBinding) (ResolvedRepo, error) {
	binding = binding.normalized()
	pds, tokens, dpop, err := r.ResolvePDS(ctx, binding)
	if err != nil {
		return ResolvedRepo{}, err
	}
	out := ResolvedRepo{Binding: binding, PDS: pds, Tokens: tokens, DPoP: dpop}
	if binding.DID != "" {
		did, err := syntax.ParseDID(binding.DID)
		if err != nil {
			return ResolvedRepo{}, fmt.Errorf("%w: repo did %q", ErrInvalidRef, binding.DID)
		}
		out.DID = did
	}
	return out, nil
}

func (r *RepoResolver) ResolveRepo(ctx context.Context, binding RepoBinding) (ResolvedRepo, error) {
	return r.Resolve(ctx, binding)
}

func (r *RepoResolver) ResolvePDS(ctx context.Context, binding RepoBinding) (PDS, TokenSource, DPoPSource, error) {
	if r == nil {
		return nil, nil, nil, fmt.Errorf("%w: repo resolver is nil", ErrInvalidConfig)
	}
	binding = binding.normalized()
	if binding.remote() {
		return r.resolveRemote(ctx, binding)
	}
	return r.resolveLocal(ctx, binding)
}

func (r *RepoResolver) resolveLocal(ctx context.Context, binding RepoBinding) (PDS, TokenSource, DPoPSource, error) {
	if r.Handlers == nil {
		return nil, nil, nil, fmt.Errorf("%w: the local handler registry is required", ErrInvalidConfig)
	}
	tokens, dpop, err := r.bindingAuth(ctx, binding, r.Tokens, r.DPoP)
	if err != nil {
		return nil, nil, nil, err
	}
	kind := binding.Kind
	if strings.TrimSpace(kind) == "" {
		kind = "pds"
	}
	endpoint := binding.Endpoint
	if endpoint == "" {
		baseURL, err := (LocalRef{Workspace: binding.Workspace, Kind: kind, Name: binding.Name}).baseURL()
		if err != nil {
			return nil, nil, nil, err
		}
		endpoint = baseURL
	} else if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	pds, err := NewLocal(LocalConfig{
		Handlers: r.Handlers,
		BaseURL:  endpoint,
		Tokens:   tokens,
		DPoP:     dpop,
		Timeout:  r.timeout(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return pds, tokens, dpop, nil
}

func (r *RepoResolver) resolveRemote(ctx context.Context, binding RepoBinding) (PDS, TokenSource, DPoPSource, error) {
	object := binding.RemotePDS
	if object == nil {
		kind := strings.ToLower(binding.Kind)
		if kind == "remotepds" || kind == "remote-pds" || kind == "remote" {
			if r.RemotePDS == nil {
				return nil, nil, nil, fmt.Errorf("%w: no RemotePDS source is configured", ErrInvalidConfig)
			}
			if binding.Name == "" {
				return nil, nil, nil, fmt.Errorf("%w: the RemotePDS binding has no name", ErrInvalidRef)
			}
			found, err := r.RemotePDS(ctx, binding.Workspace, binding.Name)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("pdsclient: reading RemotePDS %s: %w", binding.Name, err)
			}
			if found == nil {
				return nil, nil, nil, fmt.Errorf("%w: RemotePDS %s is absent", ErrInvalidRef, binding.Name)
			}
			object = found
		}
	}
	if object == nil {
		return r.resolveAnonymousRemote(ctx, binding)
	}
	spec := object.Spec
	name := object.Name
	if name == "" {
		name = binding.Name
	}
	if spec.Suspend {
		return nil, nil, nil, fmt.Errorf("%w: RemotePDS %s is suspended", ErrUnavailable, name)
	}
	tokenRef := strings.TrimSpace(spec.Auth.AccessTokenRef)
	refreshRef := strings.TrimSpace(spec.Auth.RefreshTokenRef)
	if tokenRef == "" && refreshRef == "" {
		tokenRef = strings.TrimSpace(spec.Auth.AccountRef)
	}
	if tokenRef == "" && refreshRef == "" && binding.AuthRef != "" {
		tokenRef = binding.AuthRef
	}
	if tokenRef == "" && refreshRef == "" {
		return nil, nil, nil, fmt.Errorf("%w: RemotePDS %s has no auth ref", ErrAuthenticationRequired, name)
	}
	tokens, err := r.tokenSource(ctx, tokenRef, refreshRef)
	if err != nil {
		return nil, nil, nil, err
	}
	dpop, err := r.dpopSource(ctx, strings.TrimSpace(spec.Auth.DPoPKeyRef))
	if err != nil {
		return nil, nil, nil, err
	}
	didText := strings.TrimSpace(spec.DID)
	if didText == "" {
		didText = binding.DID
	}
	var did syntax.DID
	if didText != "" {
		parsed, err := syntax.ParseDID(didText)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: RemotePDS %s has an invalid did", ErrInvalidConfig, name)
		}
		did = parsed
	}
	tlsConfig, err := r.remoteTLSConfig(spec, name)
	if err != nil {
		return nil, nil, nil, err
	}
	pds, err := NewRemote(RemoteConfig{
		BaseURL:    spec.URL,
		DID:        did,
		HTTPClient: r.HTTPClient,
		TLS:        tlsConfig,
		Tokens:     tokens,
		DPoP:       dpop,
		Timeout:    r.timeout(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return pds, tokens, dpop, nil
}

func (r *RepoResolver) resolveAnonymousRemote(ctx context.Context, binding RepoBinding) (PDS, TokenSource, DPoPSource, error) {
	endpoint := binding.Endpoint
	if endpoint == "" {
		return nil, nil, nil, fmt.Errorf("%w: the repo binding names no PDS endpoint", ErrInvalidRef)
	}
	tokens, dpop, err := r.bindingAuth(ctx, binding, r.Tokens, r.DPoP)
	if err != nil {
		return nil, nil, nil, err
	}
	parsed, err := parseBaseURL(endpoint, "http", "https")
	if err != nil {
		return nil, nil, nil, err
	}
	var did syntax.DID
	if binding.DID != "" {
		did, err = syntax.ParseDID(binding.DID)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: repo did %q", ErrInvalidRef, binding.DID)
		}
	}
	pds, err := NewRemote(RemoteConfig{BaseURL: parsed.String(), DID: did, HTTPClient: r.HTTPClient, TLS: r.TLS,
		Tokens: tokens, DPoP: dpop, Timeout: r.timeout()})
	if err != nil {
		return nil, nil, nil, err
	}
	return pds, tokens, dpop, nil
}

func (r *RepoResolver) bindingAuth(ctx context.Context, binding RepoBinding, tokens TokenSource, dpop DPoPSource) (TokenSource, DPoPSource, error) {
	ref := binding.AuthRef
	if ref == "" {
		ref = binding.SessionRef
	}
	if ref == "" {
		return tokens, dpop, nil
	}
	token, err := r.tokenSource(ctx, ref, "")
	if err != nil {
		return nil, nil, err
	}
	return token, dpop, nil
}

func (r *RepoResolver) tokenSource(ctx context.Context, accessRef, refreshRef string) (TokenSource, error) {
	if r.Secrets == nil {
		return nil, fmt.Errorf("%w: no secret resolver is configured", ErrInvalidConfig)
	}
	return TokenSourceFunc(func(ctx context.Context) (string, error) {
		if ref := strings.TrimSpace(accessRef); ref != "" {
			data, err := r.Secrets.Resolve(ctx, ref)
			if err != nil {
				return "", fmt.Errorf("pdsclient: resolving the access token ref %s: %w", ref, err)
			}
			if token := firstString(data, "accessJwt", "accessToken", "access_token", "jwt", "token", "value"); token != "" {
				return token, nil
			}
		}
		if ref := strings.TrimSpace(refreshRef); ref != "" {
			data, err := r.Secrets.Resolve(ctx, ref)
			if err != nil {
				return "", fmt.Errorf("pdsclient: resolving the refresh token ref %s: %w", ref, err)
			}
			if token := firstString(data, "refreshJwt", "refreshToken", "refresh_token", "jwt", "token", "value"); token != "" {
				return token, nil
			}
		}
		return "", fmt.Errorf("%w: the auth ref holds no token", ErrAuthenticationRequired)
	}), nil
}

func (r *RepoResolver) dpopSource(ctx context.Context, ref string) (DPoPSource, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, nil
	}
	if r.Secrets == nil {
		return nil, fmt.Errorf("%w: no secret resolver is configured", ErrInvalidConfig)
	}
	data, err := r.Secrets.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("pdsclient: resolving the dpop key ref %s: %w", ref, err)
	}
	key, err := dpopKey(data)
	if err != nil {
		return nil, err
	}
	return &dpopSigner{key: key}, nil
}

func (r *RepoResolver) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return defaultRepoTimeout
}

func (r *RepoResolver) remoteTLSConfig(spec pdsv1alpha1.RemotePDSSpec, name string) (*tls.Config, error) {
	var config *tls.Config
	if r.TLS != nil {
		config = r.TLS.Clone()
	}
	if !spec.TLS.Enabled {
		return config, nil
	}
	if config == nil {
		config = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	if ref := strings.TrimSpace(spec.TLS.CABundleRef); ref != "" {
		if r.CABundle == nil {
			return nil, fmt.Errorf("%w: RemotePDS %s needs a CA bundle resolver", ErrInvalidConfig, name)
		}
		pem, err := r.CABundle(ref)
		if err != nil {
			return nil, fmt.Errorf("pdsclient: the RemotePDS %s CA bundle %s: %w", name, ref, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%w: the RemotePDS %s CA bundle does not parse", ErrInvalidConfig, name)
		}
		config.RootCAs = pool
	}
	if serverName := strings.TrimSpace(spec.TLS.ServerName); serverName != "" {
		config.ServerName = serverName
	}
	return config, nil
}

func AuthSources(auth pdsv1alpha1.RemotePDSAuth, secrets SecretResolver) (TokenSource, DPoPSource, error) {
	accessRef := strings.TrimSpace(auth.AccessTokenRef)
	refreshRef := strings.TrimSpace(auth.RefreshTokenRef)
	if accessRef == "" && refreshRef == "" {
		accessRef = strings.TrimSpace(auth.AccountRef)
	}
	keyRef := strings.TrimSpace(auth.DPoPKeyRef)
	if accessRef == "" && refreshRef == "" && keyRef == "" {
		return nil, nil, nil
	}
	if secrets == nil {
		return nil, nil, fmt.Errorf("%w: the RemotePDS auth names refs but no secret resolver is configured", ErrInvalidConfig)
	}
	resolver := &RepoResolver{Secrets: secrets}
	var tokens TokenSource
	if accessRef != "" || refreshRef != "" {
		token, err := resolver.tokenSource(context.Background(), accessRef, refreshRef)
		if err != nil {
			return nil, nil, err
		}
		tokens = token
	}
	var dpop DPoPSource
	if keyRef != "" {
		source, err := resolver.dpopSource(context.Background(), keyRef)
		if err != nil {
			return nil, nil, err
		}
		dpop = source
	}
	return tokens, dpop, nil
}

func firstString(data map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := data[key]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case []byte:
			if strings.TrimSpace(string(typed)) != "" {
				return strings.TrimSpace(string(typed))
			}
		}
	}
	return ""
}

func dpopKey(data map[string]any) (atcrypto.PrivateKeyExportable, error) {
	if value, ok := data["dpopPrivateJwk"]; ok {
		key, err := dpopKeyValue(value)
		if err == nil {
			return key, nil
		}
	}
	for _, name := range []string{"dpopPrivateKeyMultibase", "dpop_privatekey_multibase", "privateKeyMultibase", "private_key_multibase", "dpopPrivateKey", "privateKey", "key", "value"} {
		value, ok := data[name]
		if !ok {
			continue
		}
		key, err := dpopKeyValue(value)
		if err == nil {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: the dpop key ref holds no private key", ErrInvalidConfig)
}

func dpopKeyValue(value any) (atcrypto.PrivateKeyExportable, error) {
	switch typed := value.(type) {
	case map[string]any:
		d, _ := typed["d"].(string)
		if d == "" {
			return nil, fmt.Errorf("%w: the dpop jwk has no d", ErrInvalidConfig)
		}
		return dpopKeyP256(d)
	case string:
		raw := strings.TrimSpace(typed)
		if raw == "" {
			return nil, fmt.Errorf("%w: the dpop key is empty", ErrInvalidConfig)
		}
		if strings.HasPrefix(raw, "{") {
			var jwk map[string]any
			if err := json.Unmarshal([]byte(raw), &jwk); err != nil {
				return nil, fmt.Errorf("%w: the dpop jwk: %v", ErrInvalidConfig, err)
			}
			return dpopKeyValue(jwk)
		}
		if strings.HasPrefix(raw, "z") {
			key, err := atcrypto.ParsePrivateMultibase(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: the dpop multibase key: %v", ErrInvalidConfig, err)
			}
			return key, nil
		}
		return dpopKeyP256(raw)
	}
	return nil, fmt.Errorf("%w: unsupported dpop key value", ErrInvalidConfig)
}

func dpopKeyP256(raw string) (atcrypto.PrivateKeyExportable, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(raw), "="))
	if err != nil {
		return nil, fmt.Errorf("%w: the dpop key: %v", ErrInvalidConfig, err)
	}
	key, err := atcrypto.ParsePrivateBytesP256(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: the dpop key: %v", ErrInvalidConfig, err)
	}
	return key, nil
}

type dpopSigner struct {
	key atcrypto.PrivateKeyExportable
}

func (s *dpopSigner) DPoPProof(ctx context.Context, method, target, accessToken string) (string, error) {
	if s == nil || s.key == nil {
		return "", fmt.Errorf("%w: no dpop key", ErrInvalidConfig)
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%w: the dpop target %q is not absolute", ErrInvalidRequest, target)
	}
	public, err := s.key.PublicKey()
	if err != nil {
		return "", err
	}
	jwk, err := public.JWK()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(struct {
		Typ string        `json:"typ"`
		Alg string        `json:"alg"`
		JWK *atcrypto.JWK `json:"jwk"`
	}{Typ: "dpop+jwt", Alg: "ES256", JWK: jwk})
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	htu := *parsed
	htu.RawQuery = ""
	htu.Fragment = ""
	now := time.Now()
	claims := map[string]any{
		"jti": base64.RawURLEncoding.EncodeToString(nonce),
		"htm": strings.ToUpper(method),
		"htu": htu.String(),
		"iat": now.Unix(),
		"exp": now.Add(dpopProofLifetime).Unix(),
	}
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["ath"] = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature, err := s.key.HashAndSign([]byte(signingInput))
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

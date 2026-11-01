package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
	"golang.org/x/crypto/ssh"
)

const (
	widConfigPath     = "wid/config"
	widKeysPath       = "wid/keys"
	widTokenTTL       = 5 * time.Minute
	widRotationPeriod = 24 * time.Hour
	widVerifyWindow   = 48 * time.Hour
	widMaxAudience    = 512
	widAlgorithm      = "ES256"
)

var widReservedClaims = map[string]bool{
	"iss": true, "sub": true, "aud": true, "exp": true, "nbf": true, "iat": true, "jti": true,
}

type widConfig struct {
	Issuer string `json:"issuer"`
}

type widKey struct {
	Kid       string    `json:"kid"`
	D         []byte    `json:"d"`
	CreatedAt time.Time `json:"created_at"`
	RetiredAt time.Time `json:"retired_at,omitempty"`
}

type widKeySet struct {
	Keys []widKey `json:"keys"`
}

func (k widKey) private() (*ecdsa.PrivateKey, error) {
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(k.D)
	if d.Sign() <= 0 || d.Cmp(curve.Params().N) >= 0 {
		return nil, errors.New("wid: a stored signing key is not a P-256 scalar")
	}
	priv := &ecdsa.PrivateKey{D: d}
	priv.PublicKey.Curve = curve
	priv.PublicKey.X, priv.PublicKey.Y = curve.ScalarBaseMult(k.D)
	return priv, nil
}

func newWIDKey(now time.Time) (widKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return widKey{}, err
	}
	d := make([]byte, 32)
	priv.D.FillBytes(d)
	x := make([]byte, 32)
	y := make([]byte, 32)
	priv.PublicKey.X.FillBytes(x)
	priv.PublicKey.Y.FillBytes(y)
	sum := sha256.Sum256(append(x, y...))
	return widKey{Kid: hex.EncodeToString(sum[:8]), D: d, CreatedAt: now}, nil
}

func pathWIDConfig(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "wid/config$",
		Fields: map[string]*framework.FieldSchema{
			"issuer": {Type: framework.TypeString, Description: "The issuer URL the tokens carry, served at <issuer>/.well-known/openid-configuration.", Required: true},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation:   &framework.PathOperation{Callback: b.widConfigRead},
			logical.UpdateOperation: &framework.PathOperation{Callback: b.widConfigWrite},
		},
		HelpSynopsis: "Sets the issuer of the workload identity tokens.",
	}
}

func pathWIDRotate(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "wid/rotate$",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.widRotate},
		},
		HelpSynopsis: "Rotates the workload identity signing key; the retired key verifies for 48h.",
	}
}

func pathWIDToken(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "wid/token$",
		Fields: map[string]*framework.FieldSchema{
			"role":      {Type: framework.TypeString, Description: "Name of a bound role.", Required: true},
			"challenge": {Type: framework.TypeString, Description: "A challenge issued for the role.", Required: true},
			"signature": {Type: framework.TypeString, Description: "Armored sshsig over the challenge by the bound host key, namespace prove-sshd.", Required: true},
			"audience":  {Type: framework.TypeString, Description: "The one audience of the token: an https URL, api://<api>?actx=<ctx>, a DID or a DID URL.", Required: true},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.widToken},
		},
		HelpSynopsis: "Issues an OIDC JWT for the role's workload subject on an sshsig over a challenge by the bound host key.",
	}
}

func pathWIDDiscovery(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: `wid/\.well-known/openid-configuration$`,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{Callback: b.widDiscovery},
		},
		HelpSynopsis: "The OIDC discovery document of the workload identity issuer.",
	}
}

func pathWIDKeys(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: `wid/\.well-known/keys$`,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{Callback: b.widKeys},
		},
		HelpSynopsis: "The JWK set that verifies the workload identity tokens.",
	}
}

func getWIDConfig(ctx context.Context, s logical.Storage) (widConfig, error) {
	var cfg widConfig
	raw, err := s.Get(ctx, widConfigPath)
	if err != nil || raw == nil {
		return cfg, err
	}
	err = raw.DecodeJSON(&cfg)
	return cfg, err
}

func getWIDKeys(ctx context.Context, s logical.Storage) (widKeySet, error) {
	var set widKeySet
	raw, err := s.Get(ctx, widKeysPath)
	if err != nil || raw == nil {
		return set, err
	}
	err = raw.DecodeJSON(&set)
	return set, err
}

func putWIDKeys(ctx context.Context, s logical.Storage, set widKeySet) error {
	entry, err := logical.StorageEntryJSON(widKeysPath, set)
	if err != nil {
		return err
	}
	return s.Put(ctx, entry)
}

func checkIssuer(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("issuer %q is not an http(s) URL without query or fragment", raw)
	}
	return raw, nil
}

func (b *backend) widConfigRead(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	cfg, err := getWIDConfig(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	return &logical.Response{Data: map[string]interface{}{"issuer": cfg.Issuer}}, nil
}

func (b *backend) widConfigWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	issuer, err := checkIssuer(d.Get("issuer").(string))
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	entry, err := logical.StorageEntryJSON(widConfigPath, widConfig{Issuer: issuer})
	if err != nil {
		return nil, err
	}
	b.lock.Lock()
	defer b.lock.Unlock()
	return nil, req.Storage.Put(ctx, entry)
}

func (b *backend) widRotate(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	set, err := getWIDKeys(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	set, err = rotateWIDKeys(set, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if err := putWIDKeys(ctx, req.Storage, set); err != nil {
		return nil, err
	}
	return &logical.Response{Data: map[string]interface{}{"kid": set.Keys[len(set.Keys)-1].Kid}}, nil
}

func rotateWIDKeys(set widKeySet, now time.Time) (widKeySet, error) {
	next, err := newWIDKey(now)
	if err != nil {
		return set, err
	}
	kept := []widKey{}
	for _, k := range set.Keys {
		if k.RetiredAt.IsZero() {
			k.RetiredAt = now
		}
		if now.Sub(k.RetiredAt) < widVerifyWindow {
			kept = append(kept, k)
		}
	}
	return widKeySet{Keys: append(kept, next)}, nil
}

func (b *backend) signingKey(ctx context.Context, s logical.Storage, now time.Time) (widKey, error) {
	set, err := getWIDKeys(ctx, s)
	if err != nil {
		return widKey{}, err
	}
	if n := len(set.Keys); n > 0 && set.Keys[n-1].RetiredAt.IsZero() && now.Sub(set.Keys[n-1].CreatedAt) < widRotationPeriod {
		return set.Keys[n-1], nil
	}
	set, err = rotateWIDKeys(set, now)
	if err != nil {
		return widKey{}, err
	}
	if err := putWIDKeys(ctx, s, set); err != nil {
		return widKey{}, err
	}
	return set.Keys[len(set.Keys)-1], nil
}

func checkAudience(aud string) error {
	if aud == "" || len(aud) > widMaxAudience {
		return fmt.Errorf("audience must be 1 to %d characters", widMaxAudience)
	}
	for _, c := range aud {
		if c <= 0x20 || c >= 0x7f || c == '"' || c == '\\' {
			return errors.New("audience must be printable ASCII without spaces, quotes or backslashes")
		}
	}
	switch {
	case strings.HasPrefix(aud, "did:"):
		if parts := strings.SplitN(strings.SplitN(aud, "#", 2)[0], ":", 3); len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			return errors.New("audience is not a DID or a DID URL")
		}
		return nil
	case strings.HasPrefix(aud, "api://"):
		rest := strings.TrimPrefix(aud, "api://")
		api, query, ok := strings.Cut(rest, "?")
		values, err := url.ParseQuery(query)
		if !ok || api == "" || err != nil || values.Get("actx") == "" {
			return errors.New("audience is not api://<api>?actx=<ctx>")
		}
		return nil
	case strings.HasPrefix(aud, "https://"):
		u, err := url.Parse(aud)
		if err != nil || u.Host == "" {
			return errors.New("audience is not an https URL")
		}
		return nil
	}
	return errors.New("audience must be an https URL, api://<api>?actx=<ctx>, a DID or a DID URL")
}

func (b *backend) widToken(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	in, bad := requiredStrings(d, "role", "challenge", "signature", "audience")
	if bad != nil {
		return bad, nil
	}
	name, challenge, signature, audience := in[0], in[1], in[2], in[3]
	if err := checkAudience(audience); err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	b.lock.Lock()
	defer b.lock.Unlock()
	cfg, err := getWIDConfig(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if cfg.Issuer == "" {
		return logical.ErrorResponse("the workload identity issuer is not configured yet"), logical.ErrInvalidRequest
	}
	r, err := getRole(ctx, req.Storage, name)
	if err != nil {
		return nil, err
	}
	if r == nil || r.HostKey == "" || r.WIDSubject == "" {
		return refused("token refused")
	}
	now := time.Now().UTC()
	expires, found := r.Challenges[challenge]
	pruneChallenges(r, now)
	delete(r.Challenges, challenge)
	if err := putRole(ctx, req.Storage, name, r); err != nil {
		return nil, err
	}
	if !found || !now.Before(expires) {
		return refused("token refused: unknown or expired challenge")
	}
	bound, err := r.hostKey()
	if err != nil {
		return nil, err
	}
	signer, err := verifySSHSig(signature, proveNamespace, []byte(challenge))
	if err != nil {
		return refused("token refused: " + err.Error())
	}
	if !sameKey(signer, bound) {
		return refused("token refused: the signature is not by the bound host key")
	}
	key, err := b.signingKey(ctx, req.Storage, now)
	if err != nil {
		return nil, err
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return nil, err
	}
	claims := map[string]interface{}{}
	for k, v := range r.WIDClaims {
		if !widReservedClaims[k] {
			claims[k] = v
		}
	}
	claims["iss"] = cfg.Issuer
	claims["sub"] = r.WIDSubject
	claims["aud"] = audience
	claims["iat"] = now.Unix()
	claims["nbf"] = now.Unix()
	claims["exp"] = now.Add(widTokenTTL).Unix()
	claims["jti"] = base64.RawURLEncoding.EncodeToString(jti)
	token, err := signES256(key, claims)
	if err != nil {
		return nil, err
	}
	b.Logger().Info("issued a workload identity token", "role", name, "audience", audience, "kid", key.Kid,
		"host_key_fingerprint", ssh.FingerprintSHA256(bound))
	return &logical.Response{Data: map[string]interface{}{
		"token":      token,
		"expires_at": now.Add(widTokenTTL).Format(time.RFC3339),
		"ttl":        int64(widTokenTTL.Seconds()),
	}}, nil
}

func signES256(key widKey, claims map[string]interface{}) (string, error) {
	priv, err := key.private()
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(map[string]string{"alg": widAlgorithm, "typ": "JWT", "kid": key.Kid})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(input))
	rInt, sInt, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	rInt.FillBytes(sig[:32])
	sInt.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func rawJSON(v interface{}) (*logical.Response, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &logical.Response{Data: map[string]interface{}{
		logical.HTTPContentType: "application/json",
		logical.HTTPRawBody:     body,
		logical.HTTPStatusCode:  200,
	}}, nil
}

func (b *backend) widDiscovery(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	cfg, err := getWIDConfig(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if cfg.Issuer == "" {
		return logical.ErrorResponse("the workload identity issuer is not configured yet"), nil
	}
	return rawJSON(map[string]interface{}{
		"issuer":                                cfg.Issuer,
		"jwks_uri":                              cfg.Issuer + "/.well-known/keys",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{widAlgorithm},
		"claims_supported":                      []string{"iss", "sub", "aud", "iat", "nbf", "exp", "jti", "computevm"},
	})
}

func (b *backend) widKeys(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	set, err := getWIDKeys(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	keys := []map[string]string{}
	for _, k := range set.Keys {
		if !k.RetiredAt.IsZero() && now.Sub(k.RetiredAt) >= widVerifyWindow {
			continue
		}
		priv, err := k.private()
		if err != nil {
			return nil, err
		}
		x := make([]byte, 32)
		y := make([]byte, 32)
		priv.PublicKey.X.FillBytes(x)
		priv.PublicKey.Y.FillBytes(y)
		keys = append(keys, map[string]string{
			"kty": "EC", "crv": "P-256", "use": "sig", "alg": widAlgorithm, "kid": k.Kid,
			"x": base64.RawURLEncoding.EncodeToString(x), "y": base64.RawURLEncoding.EncodeToString(y),
		})
	}
	return rawJSON(map[string]interface{}{"keys": keys})
}

package oauthclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
)

const (
	QRNSID = "com.fedfork.atprotoOauthQR"

	DefaultQRTTL = 5 * time.Minute

	QRServiceAuthTTL = time.Minute
)

type ServiceAuthSigner interface {
	SignServiceAuth(ctx context.Context, audience, lxm string, ttl time.Duration) (string, error)
}

type ServiceAuthSignerFunc func(ctx context.Context, audience, lxm string, ttl time.Duration) (string, error)

func (f ServiceAuthSignerFunc) SignServiceAuth(ctx context.Context, audience, lxm string, ttl time.Duration) (string, error) {
	if f == nil {
		return "", errors.New("oauthclient: nil service auth signer")
	}
	return f(ctx, audience, lxm, ttl)
}

type KeySigner struct {
	Issuer syntax.DID

	Key atcrypto.PrivateKey
}

func (s KeySigner) SignServiceAuth(ctx context.Context, audience, lxm string, ttl time.Duration) (string, error) {
	nsid, err := syntax.ParseNSID(lxm)
	if err != nil {
		return "", err
	}
	return serviceauth.Sign(s.Issuer, audience, nsid, s.Key, ttl)
}

type QRSession struct {
	AccessJWT string `json:"accessJwt"`

	RefreshJWT string `json:"refreshJwt"`

	UserDID string `json:"userDid"`

	Handle string `json:"handle"`

	PDS string `json:"pds"`

	DPoPPublicJWK map[string]any `json:"dpopPublicJwk"`

	DPoPPrivateJWK map[string]any `json:"dpopPrivateJwk"`
}

func QRClientID(origin string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return ""
	}
	return origin + "/oauth-client-metadata.json"
}

func jwkString(jwk map[string]any, name string) string {
	value, _ := jwk[name].(string)
	return value
}

func (q QRSession) SessionData(clientID string, scopes []string) (SessionData, error) {
	if q.UserDID == "" {
		return SessionData{}, errors.New("oauthclient: the qr session names no user DID")
	}
	did, err := syntax.ParseDID(q.UserDID)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the qr session user DID: %w", err)
	}
	if q.PDS == "" {
		return SessionData{}, errors.New("oauthclient: the qr session names no PDS")
	}
	pdsURL, err := url.Parse(q.PDS)
	if err != nil || pdsURL.Host == "" || (pdsURL.Scheme != "http" && pdsURL.Scheme != "https") {
		return SessionData{}, fmt.Errorf("oauthclient: the qr session PDS %q is not an absolute http or https URL", q.PDS)
	}
	host := strings.TrimRight(q.PDS, "/")
	key, err := privateKeyFromJWK(jwkString(q.DPoPPrivateJWK, "kty"), jwkString(q.DPoPPrivateJWK, "d"))
	if err != nil {
		return SessionData{}, err
	}
	return SessionData{
		AccountDID:              did.String(),
		SessionID:               SessionID(did.String(), host),
		HostURL:                 host,
		ClientID:                strings.TrimSpace(clientID),
		Scopes:                  append([]string(nil), scopes...),
		AccessToken:             q.AccessJWT,
		RefreshToken:            q.RefreshJWT,
		DPoPPrivateKeyMultibase: key.Multibase(),
	}, nil
}

type QRTransfer struct {
	Origin string

	CLIDID string

	Nonce string

	Signer ServiceAuthSigner

	HTTPClient *http.Client

	TTL time.Duration

	mu sync.Mutex

	consumed bool
}

func NewQRTransfer(origin, cliDID, nonce string, signer ServiceAuthSigner) (*QRTransfer, error) {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("oauthclient: the qr origin %q is not an absolute http or https URL", origin)
	}
	did, err := syntax.ParseDID(cliDID)
	if err != nil {
		return nil, fmt.Errorf("oauthclient: the qr cli DID: %w", err)
	}
	if nonce == "" {
		return nil, errors.New("oauthclient: the qr transfer names no nonce")
	}
	if signer == nil {
		return nil, errors.New("oauthclient: the qr transfer has no service auth signer")
	}
	return &QRTransfer{
		Origin: strings.TrimRight(parsed.String(), "/"),
		CLIDID: did.String(),
		Nonce:  nonce,
		Signer: signer,
		TTL:    DefaultQRTTL,
	}, nil
}

func NewQRNonce() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (q *QRTransfer) URL() (string, error) {
	if q == nil {
		return "", errors.New("oauthclient: no qr transfer")
	}
	return q.Origin + "/#oauth=" + url.QueryEscape(q.CLIDID) + "&n=" + url.QueryEscape(q.Nonce), nil
}

func (q *QRTransfer) audience() (string, error) {
	origin, err := url.Parse(q.Origin)
	if err != nil || origin.Hostname() == "" {
		return "", fmt.Errorf("oauthclient: the qr origin %q names no host", q.Origin)
	}
	return "did:web:" + strings.ToLower(origin.Hostname()), nil
}

func (q *QRTransfer) client() *http.Client {
	if q != nil && q.HTTPClient != nil {
		return q.HTTPClient
	}
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

func (q *QRTransfer) PollOnce(ctx context.Context) (QRSession, bool, error) {
	if q == nil {
		return QRSession{}, false, errors.New("oauthclient: no qr transfer")
	}
	q.mu.Lock()
	consumed := q.consumed
	q.mu.Unlock()
	if consumed {
		return QRSession{}, false, ErrQRConsumed
	}
	audience, err := q.audience()
	if err != nil {
		return QRSession{}, false, err
	}
	token, err := q.Signer.SignServiceAuth(ctx, audience, QRNSID, QRServiceAuthTTL)
	if err != nil {
		return QRSession{}, false, fmt.Errorf("oauthclient: the qr service auth token: %w", err)
	}
	query := url.Values{"cliDid": {q.CLIDID}, "nonce": {q.Nonce}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.Origin+"/xrpc/"+QRNSID+"?"+query.Encode(), nil)
	if err != nil {
		return QRSession{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return QRSession{}, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return QRSession{}, false, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		q.mu.Lock()
		q.consumed = true
		q.mu.Unlock()
		var session QRSession
		if err := json.Unmarshal(raw, &session); err != nil {
			return QRSession{}, false, fmt.Errorf("oauthclient: decoding the qr session: %w", err)
		}
		return session, true, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnauthorized:
		return QRSession{}, false, nil
	default:
		return QRSession{}, false, fmt.Errorf("oauthclient: the qr poll returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
}

func (q *QRTransfer) Publish(ctx context.Context, session QRSession) error {
	if q == nil {
		return errors.New("oauthclient: no qr transfer")
	}
	body := map[string]any{
		"cliDid":         q.CLIDID,
		"nonce":          q.Nonce,
		"accessJwt":      session.AccessJWT,
		"refreshJwt":     session.RefreshJWT,
		"userDid":        session.UserDID,
		"handle":         session.Handle,
		"pds":            session.PDS,
		"dpopPublicJwk":  session.DPoPPublicJWK,
		"dpopPrivateJwk": session.DPoPPrivateJWK,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.Origin+"/xrpc/"+QRNSID, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := q.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, ReplyLimit))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("oauthclient: the qr publish returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(reply)))
	}
	return nil
}

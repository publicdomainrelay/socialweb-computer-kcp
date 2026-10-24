package pdsclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/nettransport"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	remoteDialTimeout     = 30 * time.Second
	remoteKeepAlive       = 30 * time.Second
	remoteTLSHandshake    = 10 * time.Second
	remoteIdleConnTimeout = 90 * time.Second
	remoteMaxIdleConns    = 32
)

type RemoteConfig struct {
	BaseURL    string
	DID        syntax.DID
	HTTPClient *http.Client
	TLS        *tls.Config
	Tokens     TokenSource
	DPoP       DPoPSource
	Auth       xrpc.Auth
	Timeout    time.Duration
}

type RemotePDS struct {
	*basePDS
}

var _ PDS = (*RemotePDS)(nil)
var _ Ref = CRRef{}

func NewRemote(cfg RemoteConfig) (*RemotePDS, error) {
	baseURL, err := parseBaseURL(cfg.BaseURL, "http", "https")
	if err != nil {
		return nil, err
	}
	expectedDID := cfg.DID
	if strings.TrimSpace(expectedDID.String()) != "" {
		parsed, err := syntax.ParseDID(expectedDID.String())
		if err != nil {
			return nil, fmt.Errorf("%w: invalid PDS did", ErrInvalidConfig)
		}
		expectedDID = parsed
	} else {
		expectedDID = ""
	}
	tlsConfig := cfg.TLS
	if tlsConfig != nil {
		tlsConfig = tlsConfig.Clone()
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = newRemoteHTTPClient(tlsConfig)
	} else {
		httpClient = cloneNoRedirect(httpClient)
	}
	client := newXRPCClient(baseURL, httpClient, cfg.Tokens, cfg.DPoP, cfg.Auth, cfg.Timeout)
	client.expectedDID = expectedDID
	dial := func(ctx context.Context, cursor *int64) (RepoEventStream, error) {
		return newRemoteSubscribe(ctx, baseURL, tlsConfig, cursor)
	}
	return &RemotePDS{basePDS: &basePDS{client: client, dial: dial}}, nil
}

func (p *RemotePDS) RefreshSession(ctx context.Context, refreshToken string) (Session, error) {
	if p == nil || p.basePDS == nil || p.client == nil {
		return Session{}, fmt.Errorf("%w: client is nil", ErrInvalidConfig)
	}
	if strings.TrimSpace(refreshToken) == "" {
		return Session{}, fmt.Errorf("%w: no refresh token is available", ErrAuthenticationRequired)
	}
	var out sessionResponse
	if err := p.client.postJSON(ctx, "com.atproto.server.refreshSession", nil, bearer(refreshToken), &out); err != nil {
		return Session{}, err
	}
	session, err := sessionFromResponse(out)
	if err != nil {
		return Session{}, err
	}
	p.client.session.set(session)
	return session, nil
}

func newRemoteHTTPClient(tlsConfig *tls.Config) *http.Client {
	transport := nettransport.WithDial(&http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          remoteMaxIdleConns,
		IdleConnTimeout:       remoteIdleConnTimeout,
		TLSHandshakeTimeout:   remoteTLSHandshake,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig,
	}, (&net.Dialer{Timeout: remoteDialTimeout, KeepAlive: remoteKeepAlive}).DialContext)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

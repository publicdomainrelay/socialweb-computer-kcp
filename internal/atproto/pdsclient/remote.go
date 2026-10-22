package pdsclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

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
	client := newXRPCClient(baseURL, httpClient, cfg.Tokens, cfg.DPoP, cfg.Timeout)
	client.expectedDID = expectedDID
	dial := func(ctx context.Context, cursor *int64) (RepoEventStream, error) {
		return newRemoteSubscribe(ctx, baseURL, tlsConfig, cursor)
	}
	return &RemotePDS{basePDS: &basePDS{client: client, dial: dial}}, nil
}

func newRemoteHTTPClient(tlsConfig *tls.Config) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: remoteDialTimeout, KeepAlive: remoteKeepAlive}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          remoteMaxIdleConns,
		IdleConnTimeout:       remoteIdleConnTimeout,
		TLSHandshakeTimeout:   remoteTLSHandshake,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

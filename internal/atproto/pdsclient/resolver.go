package pdsclient

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
)

type CRRef struct {
	RemotePDS *pdsv1alpha1.RemotePDS
}

type Ref interface {
	isPDSRef()
}

func (LocalRef) isPDSRef() {}

func (CRRef) isPDSRef() {}

type Resolver struct {
	Handlers   Handlers
	HTTPClient *http.Client
	TLS        *tls.Config
	Tokens     TokenSource
	DPoP       DPoPSource
	Timeout    time.Duration
}

func (r *Resolver) Resolve(ref Ref) (PDS, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: resolver is nil", ErrInvalidConfig)
	}
	if ref == nil {
		return nil, fmt.Errorf("%w: nil reference", ErrInvalidRef)
	}
	switch value := ref.(type) {
	case LocalRef:
		return r.ResolveLocal(value)
	case CRRef:
		return r.ResolveCR(value)
	default:
		return nil, fmt.Errorf("%w: unknown reference", ErrInvalidRef)
	}
}

func (r *Resolver) ResolveLocal(ref LocalRef) (PDS, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: resolver is nil", ErrInvalidConfig)
	}
	if r.Handlers == nil {
		return nil, fmt.Errorf("%w: local handler registry is required", ErrInvalidConfig)
	}
	baseURL, err := ref.baseURL()
	if err != nil {
		return nil, err
	}
	return NewLocal(LocalConfig{
		Handlers: r.Handlers,
		BaseURL:  baseURL,
		Tokens:   r.Tokens,
		DPoP:     r.DPoP,
		Timeout:  r.Timeout,
	})
}

func (r *Resolver) ResolveCR(ref CRRef) (PDS, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: resolver is nil", ErrInvalidConfig)
	}
	if ref.RemotePDS == nil {
		return nil, fmt.Errorf("%w: RemotePDS is nil", ErrInvalidRef)
	}
	spec := ref.RemotePDS.Spec
	name := ref.RemotePDS.Name
	if spec.Suspend {
		return nil, fmt.Errorf("%w: RemotePDS %s is suspended", ErrUnavailable, name)
	}
	if strings.TrimSpace(spec.URL) == "" {
		return nil, fmt.Errorf("%w: RemotePDS %s has no URL", ErrInvalidConfig, name)
	}
	did, err := syntax.ParseDID(strings.TrimSpace(spec.DID))
	if err != nil {
		return nil, fmt.Errorf("%w: RemotePDS %s has an invalid did", ErrInvalidConfig, name)
	}
	tlsConfig, err := r.remoteTLS(spec, name)
	if err != nil {
		return nil, err
	}
	return NewRemote(RemoteConfig{
		BaseURL:    spec.URL,
		DID:        did,
		HTTPClient: r.HTTPClient,
		TLS:        tlsConfig,
		Tokens:     r.Tokens,
		DPoP:       r.DPoP,
		Timeout:    r.Timeout,
	})
}

func (r *Resolver) remoteTLS(spec pdsv1alpha1.RemotePDSSpec, name string) (*tls.Config, error) {
	var config *tls.Config
	if r.TLS != nil {
		config = r.TLS.Clone()
	} else if r.HTTPClient != nil {
		if transport, ok := r.HTTPClient.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
			config = transport.TLSClientConfig.Clone()
		}
	}
	if !spec.TLS.Enabled {
		return config, nil
	}
	if config == nil && (spec.TLS.CARef != nil || spec.TLS.CASecretRef != nil) && r.HTTPClient == nil {
		return nil, fmt.Errorf("%w: RemotePDS %s needs a TLS config for its CA bundle", ErrInvalidConfig, name)
	}
	if config == nil {
		config = &tls.Config{}
	}
	if serverName := strings.TrimSpace(spec.TLS.ServerName); serverName != "" {
		config.ServerName = serverName
	}
	return config, nil
}

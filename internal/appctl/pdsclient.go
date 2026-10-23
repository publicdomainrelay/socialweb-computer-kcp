package appctl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
)

const pdsClientTimeout = 30 * time.Second

func (ctl *Controller) pdsFor(endpoint, caPEM string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if resolver := ctl.c.RepoResolver; resolver != nil {
		binding := pdsclient.RepoBinding{Workspace: ctl.c.workspace(), Endpoint: endpoint}
		if remote := ctl.c.RemotePDS; remote != nil {
			binding.Kind = "RemotePDS"
			binding.Name = remote.Name
			binding.DID = remote.Spec.DID
			binding.RemotePDS = remote
		} else if strings.HasPrefix(endpoint, "https://") {
			binding.Kind = "url"
			binding.Endpoint = endpoint
		} else if endpoint != "" {
			binding.Kind = "pds"
			binding.Name = PdsComponent
			binding.Endpoint = endpoint
		} else if ctl.c.Handlers != nil {
			binding.Kind = ComponentKindPds
			binding.Name = PdsComponent
		} else {
			return pdsClientAt(endpoint, caPEM)
		}
		pds, _, _, err := resolver.ResolvePDS(context.Background(), binding)
		return pds, err
	}
	if ctl.c.RemotePDS != nil {
		tlsConfig, err := remotePDSTLS(ctl.c.RemotePDS.Spec.TLS, caPEM)
		if err != nil {
			return nil, err
		}
		resolver := &pdsclient.Resolver{Handlers: ctl.c.Handlers, TLS: tlsConfig, Timeout: pdsClientTimeout}
		return resolver.Resolve(pdsclient.CRRef{RemotePDS: ctl.c.RemotePDS})
	}
	if ctl.c.Handlers != nil && !strings.HasPrefix(endpoint, "https://") {
		resolver := &pdsclient.Resolver{Handlers: ctl.c.Handlers, Timeout: pdsClientTimeout}
		return resolver.ResolveLocal(pdsclient.LocalRef{Workspace: ctl.c.workspace(), Kind: ComponentKindPds, Name: PdsComponent, Endpoint: endpoint})
	}
	return pdsClientAt(endpoint, caPEM)
}

func (ctl *Controller) pdsForToken(endpoint, caPEM, token string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	tokens := pdsclient.TokenSourceFunc(func(context.Context) (string, error) { return token, nil })
	if ctl.c.Handlers != nil && !strings.HasPrefix(endpoint, "https://") {
		transport := &http.Transport{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return ctl.c.Handlers.DialHTTP(ctx, address)
		}}
		return pdsclient.NewRemote(pdsclient.RemoteConfig{
			BaseURL:    endpoint,
			HTTPClient: &http.Client{Transport: transport, Timeout: pdsClientTimeout},
			Tokens:     tokens,
			Timeout:    pdsClientTimeout,
		})
	}
	tlsConfig, err := pdsTLS(endpoint, caPEM)
	if err != nil {
		return nil, err
	}
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, TLS: tlsConfig, Tokens: tokens, Timeout: pdsClientTimeout})
}

func pdsClientAt(endpoint, caPEM string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("appctl: no PDS endpoint")
	}
	tlsConfig, err := pdsTLS(endpoint, caPEM)
	if err != nil {
		return nil, err
	}
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, TLS: tlsConfig, Timeout: pdsClientTimeout})
}

func pdsTLS(endpoint, caPEM string) (*tls.Config, error) {
	if strings.TrimSpace(caPEM) == "" || !strings.HasPrefix(endpoint, "https://") {
		return nil, nil
	}
	pool, err := pdsCertPool(caPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool}, nil
}

func remotePDSTLS(spec pdsv1alpha1.RemotePDSTLS, fallbackCA string) (*tls.Config, error) {
	caPEM := strings.TrimSpace(fallbackCA)
	if ref := strings.TrimSpace(spec.CABundleRef); ref != "" {
		raw, err := os.ReadFile(openbaoctl.FileRef(ref))
		if err != nil {
			return nil, err
		}
		caPEM = string(raw)
	}
	var config *tls.Config
	if caPEM != "" {
		pool, err := pdsCertPool(caPEM)
		if err != nil {
			return nil, err
		}
		config = &tls.Config{RootCAs: pool}
	}
	if name := strings.TrimSpace(spec.ServerName); name != "" {
		if config == nil {
			config = &tls.Config{}
		}
		config.ServerName = name
	}
	return config, nil
}

func pdsCertPool(caPEM string) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("the CA bundle does not parse")
	}
	return pool, nil
}

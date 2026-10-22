package appctl

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

const pdsClientTimeout = 30 * time.Second

func (ctl *Controller) pdsFor(endpoint, caFile string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if ctl.c.RemotePDS != nil {
		tlsConfig, err := remotePDSTLS(ctl.c.RemotePDS.Spec.TLS, caFile)
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
	return pdsClientAt(endpoint, caFile)
}

func pdsClientAt(endpoint, caFile string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("appctl: no PDS endpoint")
	}
	tlsConfig, err := pdsTLS(endpoint, caFile)
	if err != nil {
		return nil, err
	}
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, TLS: tlsConfig, Timeout: pdsClientTimeout})
}

func pdsTLS(endpoint, caFile string) (*tls.Config, error) {
	if strings.TrimSpace(caFile) == "" || !strings.HasPrefix(endpoint, "https://") {
		return nil, nil
	}
	pool, err := pdsCertPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{RootCAs: pool}, nil
}

func remotePDSTLS(spec pdsv1alpha1.RemotePDSTLS, fallbackCA string) (*tls.Config, error) {
	ref := strings.TrimSpace(spec.CABundleRef)
	if ref == "" {
		ref = strings.TrimSpace(fallbackCA)
	}
	var config *tls.Config
	if ref != "" {
		pool, err := pdsCertPool(fileRef(ref))
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

func pdsCertPool(caFile string) (*x509.CertPool, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("the CA bundle %s does not parse", caFile)
	}
	return pool, nil
}

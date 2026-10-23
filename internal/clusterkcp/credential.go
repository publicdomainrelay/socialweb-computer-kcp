package clusterkcp

import (
	"context"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

func RestConfig(cred *clusterprovider.Credential) *rest.Config {
	if cred == nil {
		return nil
	}
	return &rest.Config{
		Host: cred.Host,
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   cred.CAData,
			CertData: cred.CertData,
			KeyData:  cred.KeyData,
		},
	}
}

func restConfig(cred *clusterprovider.Credential, err error) (*rest.Config, error) {
	if err != nil {
		return nil, err
	}
	return RestConfig(cred), nil
}

func MintTenantCredentialForCluster(ctx context.Context, logins *openbao.Logins, stateDir, cluster, server string, source clusterprovider.CredentialSource) (*rest.Config, error) {
	return restConfig(clusterprovider.MintTenantCredentialForCluster(ctx, logins, stateDir, cluster, server, source))
}

func MintTenantCredentialForClusterAs(ctx context.Context, stateDir, cluster, server, commonName string, organizations []string, validFor time.Duration) (*rest.Config, error) {
	return restConfig(clusterprovider.MintTenantCredentialForClusterAs(ctx, stateDir, cluster, server, commonName, organizations, validFor))
}

func MintTenantCredentialForDataDirAs(dataDir, server, commonName string, organizations []string, validFor time.Duration) (*rest.Config, error) {
	return restConfig(clusterprovider.MintTenantCredentialForDataDirAs(dataDir, server, commonName, organizations, validFor))
}

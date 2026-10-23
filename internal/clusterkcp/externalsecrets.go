package clusterkcp

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eso"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const externalSecretsKubeconfigContext = "socialweb-external-secrets"

func ExternalSecrets(binary, bundle, legacyNamespace string) *clusterprovider.ExternalSecrets {
	return &clusterprovider.ExternalSecrets{
		Binary:          binary,
		Bundle:          bundle,
		LegacyNamespace: legacyNamespace,
		WebhookCAName:   eso.WebhookCAName,
		Install:         InstallExternalSecrets,
	}
}

func InstallExternalSecrets(ctx context.Context, o clusterprovider.ExternalSecretsInstall) error {
	legacy := cmp.Or(o.LegacyNamespace, eso.DefaultControllerNamespace)
	minted, err := clusterprovider.MintTenantCredentialForClusterAs(ctx, o.StateDir, o.Cluster, o.APIServer,
		eso.ControllerUser, []string{eso.ControllerGroup}, clusterprovider.DefaultCredentialValidity)
	if err != nil {
		return fmt.Errorf("clusterprovider: minting the credential the External Secrets "+
			"controller of %s authenticates with: %w", o.Cluster, err)
	}
	if err := WriteKubeconfig(clusterstack.ExternalSecretsKubeconfig(o.DataDir), minted,
		externalSecretsKubeconfigContext, clusterprovider.ErrExternalSecretsKubeconfigUnwritable); err != nil {
		return err
	}
	caPEM, err := eso.EnsureWebhookTLS(eso.WebhookTLSPaths{
		CertDir: clusterstack.ExternalSecretsCertDir(o.DataDir),
		CAKey:   clusterstack.ExternalSecretsCAKey(o.DataDir),
	}, time.Now())
	if err != nil {
		return err
	}
	kc, err := adminFromDataDir(o.StateDir, o.Cluster, o.APIServer)
	if err != nil {
		return fmt.Errorf("clusterprovider: the admin client that installs the External "+
			"Secrets CRDs into %s: %w", o.Cluster, err)
	}
	if err := eso.InstallBeside(ctx, kc, eso.BesideOptions{
		BundlePath:      o.Bundle,
		LegacyNamespace: legacy,
		WebhookURL:      clusterprovider.ExternalSecretsWebhookURL(o.WebhookPort),
		WebhookCAPEM:    caPEM,
		FieldManager:    clusterprovider.AddOnFieldManager,
	}); err != nil {
		return fmt.Errorf("clusterprovider: installing the tenant half of the External "+
			"Secrets controller of %s: %w", o.Cluster, err)
	}
	return nil
}

func adminFromDataDir(stateDir, cluster, apiserver string) (*kube.Client, error) {
	dataDir, err := clusterprovider.ClusterDataDir(stateDir, cluster)
	if err != nil {
		return nil, err
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", clusterstack.AdminKubeconfig(dataDir))
	if err != nil {
		return nil, fmt.Errorf("clusterprovider: the cluster's own admin credential: %w", err)
	}
	if apiserver != "" {
		cfg.Host = apiserver
	}
	return kube.New(kcpserver.Throughput(cfg))
}

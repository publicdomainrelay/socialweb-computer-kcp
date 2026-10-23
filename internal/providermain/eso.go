package providermain

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterkcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/esovault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tenantnet"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const tenantAddOnTimeout = 60 * time.Second

func externalSecretsFor(cfg Config) *clusterprovider.ExternalSecrets {
	return clusterkcp.ExternalSecrets(cfg.ExternalSecrets, cfg.ESOBundle, cfg.ESONamespace)
}

func tenantAddOns(cfg Config, logins *openbao.Logins, log *slog.Logger) func(context.Context, string, *clusterprovider.Credential) error {
	return func(ctx context.Context, cluster string, cred *clusterprovider.Credential) error {
		restCfg := clusterkcp.RestConfig(cred)
		dataDir, err := clusterprovider.ClusterDataDir(cfg.StateDir, cluster)
		if err != nil {
			return err
		}
		kc, err := kube.New(kcpserver.Throughput(rest.CopyConfig(restCfg)))
		if err != nil {
			return fmt.Errorf("clusterprovider: a dynamic client for %s's tenant add-ons: %w", cluster, err)
		}
		netCtx, netCancel := context.WithTimeout(ctx, tenantAddOnTimeout)
		err = tenantnet.Apply(netCtx, kc, clusterprovider.AddOnFieldManager)
		netCancel()
		if err != nil {
			return fmt.Errorf("clusterprovider: the node network add-ons of %s: %w", cluster, err)
		}
		log.Info("cluster provider: applied the node network add-ons",
			"cluster", cluster, "nodeReader", tenantnet.NodeReaderName, "nodeDNS", tenantnet.DNSName,
			"address", tenantnet.Address, "nodeLabel", tenantnet.NodeLabel,
			"meaning", "every node may list Nodes for socialweb-meshd, and a node whose user_data carries "+
				"the link-local DNS address runs its own CoreDNS there; both are ours and baseline",
			"item", "plan 0005 M5-DNS, M5-MESHD")
		bounded, cancel := context.WithTimeout(ctx, tenantAddOnTimeout)
		defer cancel()
		client, access, err := clusterprovider.ClusterVaultClient(bounded, logins, dataDir)
		if err != nil {
			return fmt.Errorf("clusterprovider: logging in to the cluster vault for the tenant add-ons of %s: %w", cluster, err)
		}
		vaultURL, err := esovault.ApplyVaultStore(bounded, kc, esovault.VaultStore{
			Cluster:     cluster,
			DataDir:     dataDir,
			VaultPort:   access.Port(),
			Client:      client,
			ManifestDir: cfg.ESOManifestDir,
			Namespace:   kcpclient.TenantNamespace,
		})
		if err != nil {
			return err
		}
		cs, err := kubernetes.NewForConfig(restCfg)
		if err != nil {
			return fmt.Errorf("clusterprovider: a client to measure the add-on baseline of %s: %w", cluster, err)
		}
		if err := measureTenantBaseline(ctx, cs, dataDir, cluster, log); err != nil {
			return err
		}
		log.Info("cluster provider: applied the tenant add-ons",
			"cluster", cluster, "manifests", cfg.ESOManifestDir,
			"namespace", kcpclient.TenantNamespace, "vault", vaultURL,
			"decision", "the External Secrets controller runs beside the control plane in this "+
				"cluster's process group, so the store dials the cluster vault on loopback and "+
				"syncs at zero nodes; the tenant holds only its CRDs and RBAC",
			"item", "NEXT.yaml ESO-4")
		return nil
	}
}

func measureTenantBaseline(ctx context.Context, cs kubernetes.Interface, dataDir, cluster string, log *slog.Logger) error {
	b, err := clusterkcp.MeasureAddOnBaseline(ctx, cs, clusterprovider.AddOnFieldManager, time.Now())
	if err != nil {
		return fmt.Errorf("clusterprovider: measuring the add-on baseline of %s: %w", cluster, err)
	}
	if err := clusterprovider.WriteAddOnBaseline(dataDir, b); err != nil {
		return err
	}
	names := make([]string, 0, len(b.Workloads))
	for _, w := range b.Workloads {
		names = append(names, w.WorkloadKey.String())
	}
	log.Info("cluster provider: measured the add-on baseline",
		"cluster", cluster, "manager", b.Manager, "workloads", names,
		"path", clusterprovider.AddOnBaselinePath(dataDir),
		"meaning", "a cluster holding exactly these workloads, with these specs, has nothing deployed "+
			"and scales to zero nodes; a changed spec or any other workload is a tenant's intent")
	return nil
}

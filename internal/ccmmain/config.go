package ccmmain

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
)

const (
	envTenantKubeconfig = "SOCIALWEB_CLOUDCONTROLLERMANAGER_TENANT_KUBECONFIG"

	envCluster = "SOCIALWEB_CLOUDCONTROLLERMANAGER_CLUSTER"

	envInstancesFile = "SOCIALWEB_CLOUDCONTROLLERMANAGER_INSTANCES_FILE"

	envMaxNodes = "SOCIALWEB_CLOUDCONTROLLERMANAGER_MAX_NODES"

	envPodsPerNode = "SOCIALWEB_CLOUDCONTROLLERMANAGER_PODS_PER_NODE"
)

const (
	DefaultMaxNodes = 4

	DefaultPodsPerNode = 8
)

type Config struct {
	TenantKubeconfig string

	Cluster string

	InstancesFile string

	MaxNodes int

	PodsPerNode int
}

func load(get func(string) string) (Config, error) {
	cfg := Config{
		TenantKubeconfig: strings.TrimSpace(get(envTenantKubeconfig)),
		Cluster:          strings.TrimSpace(get(envCluster)),
		InstancesFile:    strings.TrimSpace(get(envInstancesFile)),
		MaxNodes:         DefaultMaxNodes,
		PodsPerNode:      DefaultPodsPerNode,
	}

	for _, req := range []struct{ name, value string }{
		{envTenantKubeconfig, cfg.TenantKubeconfig},
		{envCluster, cfg.Cluster},
		{envInstancesFile, cfg.InstancesFile},
	} {
		if req.value == "" {
			return Config{}, fmt.Errorf(
				"%s is not set. The cloud controller manager is a peer of this cluster's control "+
					"plane: it holds a tenant credential for it, it owns a cluster name, and it "+
					"reads the instances that back the Nodes in its own state directory", req.name)
		}
	}

	if err := checkTenantKubeconfig(cfg.TenantKubeconfig); err != nil {
		return Config{}, err
	}

	if raw := strings.TrimSpace(get(envMaxNodes)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s is %q, which is not a whole number: %w", envMaxNodes, raw, err)
		}
		if n < 1 {
			return Config{}, fmt.Errorf(
				"%s is %q. A ceiling below one node cannot hold any work, so a cluster with any "+
					"intent could never be satisfied", envMaxNodes, raw)
		}
		cfg.MaxNodes = n
	}

	if raw := strings.TrimSpace(get(envPodsPerNode)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s is %q, which is not a whole number: %w", envPodsPerNode, raw, err)
		}
		if n < 1 {
			return Config{}, fmt.Errorf(
				"%s is %q. The autoscaling signal divides work by this number, so a value below "+
					"one is not a smaller node, it is no ratio at all", envPodsPerNode, raw)
		}
		cfg.PodsPerNode = n
	}

	return cfg, nil
}

func checkTenantKubeconfig(path string) error {
	if filepath.Base(path) == clusterstack.AdminKubeconfigName {
		return fmt.Errorf(
			"%s is %q, which is k3s's OWN admin kubeconfig, and it is refused. Invariant 20: the "+
				"provider's tenant credential is MINTED, never copied -- a client certificate "+
				"signed by this cluster's own ClientCA and valid only against this cluster. The "+
				"k3s-written admin kubeconfig is shared state on a multi-cluster host and its "+
				"server name is unverified against the address a node dials, so the cloud "+
				"controller manager must not be given it. Point %s at the minted tenant kubeconfig "+
				"produced by internal/clusterprovider/tenantcredential.go",
			envTenantKubeconfig, path, envTenantKubeconfig)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf(
				"%s is %q, and no such file exists. A controller with no tenant credential cannot "+
					"read the Node objects it reconciles", envTenantKubeconfig, path)
		}
		return fmt.Errorf("%s is %q, which cannot be read: %w", envTenantKubeconfig, path, err)
	}

	lower := strings.ToLower(string(raw))
	for _, forbidden := range forbiddenInTenantKubeconfig {
		if strings.Contains(lower, strings.ToLower(forbidden)) {
			return fmt.Errorf(
				"%s is %q and its document contains %q. The tenant credential is a minted client "+
					"certificate, never a static bearer token and never k3s's admin kubeconfig; a "+
					"document carrying one of those is refused rather than used",
				envTenantKubeconfig, path, forbidden)
		}
	}
	return nil
}

var forbiddenInTenantKubeconfig = []string{"bearer_token", "token:", "admin.kubeconfig"}

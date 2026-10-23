package clusterprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const CloudControllerManagerName = "system:socialweb:cloud-controller-manager"

const CloudControllerManagerGroup = "system:socialweb:cloud-controller-manager"

const CloudControllerManagerClusterRole = "socialweb-cloud-controller-manager"

const CloudControllerManagerClusterRoleBinding = "socialweb-cloud-controller-manager"

const cloudControllerManagerKubeconfigContext = "socialweb-cloud-controller-manager"

var ErrNoCloudControllerManagerSeam = errors.New(
	"clusterprovider: this Stack puts no k3s step in the group, so the cloud controller manager's " +
		"minted credential has no moment after the cluster's CAs exist to be written in, and the " +
		"controller would start against a kubeconfig that does not exist -- which the process would " +
		"report as a missing file, not as a missing seam")

var ErrNoCloudControllerManagerCredential = errors.New(
	"clusterprovider: the cloud controller manager needs a credential minted for it and no admin " +
		"client was supplied to grant that identity its RBAC. Invariant 20 forbids the controller " +
		"the k3s admin kubeconfig, and a scoped identity with no binding is authenticated and denied, " +
		"so the controller would come up and never sync its informer")

var ErrCloudControllerManagerKubeconfigUnwritable = errors.New(
	"clusterprovider: the cloud controller manager's minted tenant kubeconfig could not be put on " +
		"disk before its step ran. The controller takes that file as its only credential and refuses " +
		"to start without it, so a write that failed quietly would leave the component failing at " +
		"start for a reason that is not in the controller's own log")

func prepareCloudControllerManager(steps []procgroup.Step, cfg StackConfig, cluster, apiserver string) ([]procgroup.Step, error) {
	idx := -1
	for i, s := range steps {
		if s.Name == procgroup.ComponentK3s {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNoCloudControllerManagerSeam
	}
	if cfg.Admin == nil {
		return nil, ErrNoCloudControllerManagerCredential
	}
	dataDir, err := ClusterDataDir(cfg.StateDir, cluster)
	if err != nil {
		return nil, err
	}
	kubeconfig := clusterstack.CloudControllerManagerKubeconfig(dataDir)
	inner := steps[idx].Ready
	steps[idx].Ready = func(ctx context.Context, p procgroup.Member) error {
		if inner != nil {
			if err := inner(ctx, p); err != nil {
				return err
			}
		}
		minted, err := MintTenantCredentialForClusterAs(ctx, cfg.StateDir, cluster, apiserver,
			CloudControllerManagerName, []string{CloudControllerManagerGroup}, DefaultCredentialValidity)
		if err != nil {
			return fmt.Errorf("clusterprovider: minting the scoped credential the cloud controller "+
				"manager of %s authenticates with: %w", cluster, err)
		}
		if err := cfg.writeKubeconfig(kubeconfig, minted, cloudControllerManagerKubeconfigContext,
			ErrCloudControllerManagerKubeconfigUnwritable); err != nil {
			return err
		}
		core, err := cfg.Admin(ctx, cluster, apiserver)
		if err != nil {
			return err
		}
		if core == nil {
			return fmt.Errorf("%w (cluster %s)", ErrNoCloudControllerManagerCredential, cluster)
		}
		return core.GrantCloudControllerManager(ctx, cluster)
	}
	return steps, nil
}

func (cfg StackConfig) writeKubeconfig(path string, cred *Credential, contextName string, unwritable error) error {
	if cfg.WriteKubeconfig == nil {
		return fmt.Errorf("%w: no kubeconfig writer was supplied for %s", unwritable, path)
	}
	return cfg.WriteKubeconfig(path, cred, contextName, unwritable)
}

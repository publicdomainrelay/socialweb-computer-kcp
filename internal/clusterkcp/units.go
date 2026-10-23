package clusterkcp

import (
	"cmp"
	"errors"
	"strings"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
)

type UnitsConfig struct {
	RestConfig *rest.Config

	Host string

	Workspace string

	Units clusterprovider.UnitsConfig
}

func NewUnitsHost(c UnitsConfig) (*clusterprovider.UnitsHost, error) {
	if c.RestConfig == nil || c.Host == "" {
		return nil, errors.New("clusterprovider: the units host needs the kcp the HostProcess supervisor watches")
	}
	workspace := cmp.Or(c.Workspace, hostprocess.DefaultWorkspace)
	cfg := rest.CopyConfig(c.RestConfig)
	cfg.Host = strings.TrimSuffix(c.Host, "/") + "/clusters/" + workspace
	kc, err := kube.New(cfg)
	if err != nil {
		return nil, err
	}
	u := c.Units
	u.HostProcesses = NewObjects[v1alpha1.HostProcess](kc.Resource(v1alpha1.HostProcessGVR))
	u.Postgres = NewObjects[v1alpha1.Postgres](kc.Resource(v1alpha1.PostgresGVR))
	u.OpenBaoServers = NewObjects[v1alpha1.OpenBaoServer](kc.Resource(v1alpha1.OpenBaoServerGVR))
	return clusterprovider.NewUnitsHost(u)
}

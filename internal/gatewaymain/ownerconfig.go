package gatewaymain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterkcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tenantapply"
)

const ownerCredentialValidity = 10 * time.Minute

func ownerCredentials(stateDir string) tenantapply.OwnerConfig {
	return func(_ context.Context, did, cluster string) (*rest.Config, error) {
		if did == "" || cluster == "" {
			return nil, fmt.Errorf("an owner credential is for one cluster of one account, and this "+
				"lookup names account %q and cluster %q", did, cluster)
		}
		dir, err := clientcert.DataDir(stateDir, did, cluster)
		if err != nil {
			return nil, err
		}
		raw, err := os.ReadFile(clusterprovider.FrontStatePath(dir))
		if err != nil {
			return nil, fmt.Errorf("the apiserver front of cluster %q is not reporting an address: %w", cluster, err)
		}
		var front struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(raw, &front); err != nil || front.Address == "" {
			return nil, fmt.Errorf("the apiserver front of cluster %q reports no address", cluster)
		}
		return clusterkcp.MintTenantCredentialForDataDirAs(dir, "https://"+front.Address, did,
			[]string{kcpclient.OwnersGroup}, ownerCredentialValidity)
	}
}

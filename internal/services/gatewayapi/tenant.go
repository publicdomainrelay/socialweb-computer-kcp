package gatewayapi

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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tenantapply"
)

const ownerCredentialValidity = 10 * time.Minute

func newTenantAPI(stateDir string) (gateway.TenantAPI, error) {
	if stateDir == "" {
		return nil, nil
	}
	return tenantapply.New(ownerCredentials(stateDir))
}

func ownerCredentials(stateDir string) func(context.Context, string, string) (*rest.Config, error) {
	return func(_ context.Context, did, cluster string) (*rest.Config, error) {
		if did == "" || cluster == "" {
			return nil, fmt.Errorf("an owner credential is for one cluster of one account, and this lookup names account %q and cluster %q", did, cluster)
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

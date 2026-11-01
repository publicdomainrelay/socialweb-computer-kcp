package gatewayapi

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/custody"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

var openBaoServerGVR = v1alpha1.OpenBaoServerGVR

const KindVault = "Vault"

type vaultRuntime struct {
	mount string

	endpoint custody.Endpoint
}

func (s *Service) resolveVault(ctx context.Context) (*vaultRuntime, error) {
	if !s.vaultConfigured() {
		return nil, nil
	}
	name := s.spec.VaultRef.Name
	workspace := s.workspace
	if strings.EqualFold(s.spec.VaultRef.Kind, "RemoteOpenBao") {
		return nil, fmt.Errorf("%w: %s/%s", component.ErrRemoteVaultDisabled, workspace, name)
	}
	store, err := kcpStoreFor(s.deps.KCP, workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the vault store: %w", err)
	}
	servers := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, openBaoServerGVR)
	server, err := servers.Get(ctx, ref.New(workspace, "", name))
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: reading OpenBaoServer %s/%s: %w", workspace, name, err)
	}
	status := server.Status
	endpoint, client, err := custody.EndpointFor(status, DefaultVaultClient, DefaultAtprotoMount)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: OpenBaoServer %s/%s: %w", workspace, name, err)
	}
	return &vaultRuntime{mount: strings.Trim(cmp.Or(client.KVMount, status.Refs.KVMount), "/"), endpoint: endpoint}, nil
}

func (s *Service) vaultRef() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindVault, Name: s.spec.VaultRef.Name, Workspace: cmp.Or(s.spec.VaultRef.Workspace, s.workspace)}
}

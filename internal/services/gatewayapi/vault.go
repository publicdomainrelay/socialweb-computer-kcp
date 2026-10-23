package gatewayapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
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
	if status.Phase != v1alpha1.OpenBaoServerPhaseReady || status.Address == "" || status.Refs == nil {
		return nil, fmt.Errorf("gatewayapi: OpenBaoServer %s/%s is %s, not Ready", workspace, name, status.Phase)
	}
	client := pickVaultClient(status.Clients, DefaultVaultClient)
	if client == nil {
		return nil, fmt.Errorf("gatewayapi: OpenBaoServer %s/%s reports no client %q", workspace, name, DefaultVaultClient)
	}
	ca, err := readRef(status.Refs.CA)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the OpenBao CA: %w", err)
	}
	cert, err := readRef(client.ClientCert)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the OpenBao client certificate: %w", err)
	}
	key, err := readRef(client.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the OpenBao client key: %w", err)
	}
	mount := cmp.Or(client.KVMount, status.Refs.KVMount)
	return buildVault(ctx, status.Address, ca, cert, key, client.CertAuthMount, client.CertAuthRole, mount)
}

func pickVaultClient(clients []v1alpha1.OpenBaoServerClientRefs, name string) *v1alpha1.OpenBaoServerClientRefs {
	if name != "" {
		for i := range clients {
			if clients[i].Name == name {
				return &clients[i]
			}
		}
		return nil
	}
	if len(clients) == 1 {
		return &clients[0]
	}
	return nil
}

func readRef(path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("the ref is empty")
	}
	return os.ReadFile(strings.TrimPrefix(path, "file:"))
}

func buildVault(_ context.Context, address string, ca, cert, key []byte, certMount, certRole, mount string) (*vaultRuntime, error) {
	return &vaultRuntime{
		mount: strings.Trim(mount, "/"),
		endpoint: custody.Endpoint{
			Address:    address,
			CACert:     append([]byte(nil), ca...),
			ClientCert: append([]byte(nil), cert...),
			ClientKey:  append([]byte(nil), key...),
			CertMount:  certMount,
			CertRole:   certRole,
			Mount:      DefaultAtprotoMount,
			Timeout:    DefaultCustodyTimeout,
		},
	}, nil
}

func (s *Service) vaultRef() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindVault, Name: s.spec.VaultRef.Name, Workspace: cmp.Or(s.spec.VaultRef.Workspace, s.workspace)}
}

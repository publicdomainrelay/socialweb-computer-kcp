package gatewayapi

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbao"
)

var openBaoServerGVR = v1alpha1.OpenBaoServerGVR

type vaultRuntime struct {
	client *openbao.Client

	mount string

	endpoint custodyEndpoint
}

func resolveVault(ctx context.Context, cfg Config, deps component.Deps) (*vaultRuntime, error) {
	if !cfg.Vault.configured() {
		return nil, nil
	}
	if cfg.Vault.Client != nil {
		endpoint := custodyFromConfig(cfg.Vault)
		endpoint.Mount = cfg.Session.CustodyMount
		return &vaultRuntime{client: cfg.Vault.Client, mount: strings.Trim(cfg.Vault.Mount, "/"), endpoint: endpoint}, nil
	}
	if cfg.Vault.Address != "" {
		return buildVault(ctx, cfg, cfg.Vault.Address, cfg.Vault.Token, cfg.Vault.CACert, cfg.Vault.ClientCert, cfg.Vault.ClientKey,
			cfg.Vault.CertAuthMount, cfg.Vault.CertAuthRole, cfg.Vault.Mount)
	}
	workspace := cfg.Vault.Ref.Workspace
	if workspace == "" {
		workspace = cfg.Workspace
	}
	if strings.EqualFold(cfg.Vault.Ref.Kind, "RemoteOpenBao") {
		return nil, fmt.Errorf("%w: %s/%s", component.ErrRemoteVaultDisabled, workspace, cfg.Vault.Ref.Name)
	}
	if workspace == "" {
		return nil, errors.New("gatewayapi: the vault ref has no workspace")
	}
	store, err := kcpStoreFor(deps, workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the vault store: %w", err)
	}
	servers := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, openBaoServerGVR)
	server, err := servers.Get(ctx, ref.New(workspace, "", cfg.Vault.Ref.Name))
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: reading OpenBaoServer %s/%s: %w", workspace, cfg.Vault.Ref.Name, err)
	}
	status := server.Status
	if status.Phase != v1alpha1.OpenBaoServerPhaseReady || status.Address == "" || status.Refs == nil {
		return nil, fmt.Errorf("gatewayapi: OpenBaoServer %s/%s is %s, not Ready", workspace, cfg.Vault.Ref.Name, status.Phase)
	}
	client := pickVaultClient(status.Clients, cfg.Vault.ClientName)
	if client == nil {
		return nil, fmt.Errorf("gatewayapi: OpenBaoServer %s/%s reports no client %q", workspace, cfg.Vault.Ref.Name, cfg.Vault.ClientName)
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
	certMount := cfg.Vault.CertAuthMount
	if certMount == "" {
		certMount = client.CertAuthMount
	}
	certRole := cfg.Vault.CertAuthRole
	if certRole == "" {
		certRole = client.CertAuthRole
	}
	mount := cfg.Vault.Mount
	if mount == "" {
		mount = client.KVMount
	}
	if mount == "" {
		mount = status.Refs.KVMount
	}
	return buildVault(ctx, cfg, status.Address, cfg.Vault.Token, ca, cert, key, certMount, certRole, mount)
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

func buildVault(ctx context.Context, cfg Config, address, token string, ca, cert, key []byte, certMount, certRole, mount string) (*vaultRuntime, error) {
	anonymous, err := newOpenBaoClient(address, "", ca, cert, key)
	if err != nil {
		return nil, err
	}
	client := anonymous
	switch {
	case token == "" && certMount != "" && certRole != "":
		auth, err := anonymous.LoginCertToken(ctx, certMount, certRole)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: logging in to the vault as %s/%s: %w", certMount, certRole, err)
		}
		client = anonymous.WithToken(auth.Token)
		go renewVaultToken(ctx, client, certMount, certRole, auth.Lease, cfg.Log)
	case token != "":
		client, err = newOpenBaoClient(address, token, ca, cert, key)
		if err != nil {
			return nil, err
		}
	}
	return &vaultRuntime{
		client: client,
		mount:  strings.Trim(mount, "/"),
		endpoint: custodyEndpoint{
			Address:    address,
			CACert:     append([]byte(nil), ca...),
			ClientCert: append([]byte(nil), cert...),
			ClientKey:  append([]byte(nil), key...),
			CertMount:  certMount,
			CertRole:   certRole,
			Mount:      cfg.Session.CustodyMount,
			Timeout:    cfg.Vault.Timeout,
		},
	}, nil
}

func renewVaultToken(ctx context.Context, client *openbao.Client, mount, role string, lease time.Duration, log *slog.Logger) {
	if lease <= 0 {
		return
	}
	timer := time.NewTimer(renewTokenDelay(lease))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			auth, err := client.LoginCertToken(ctx, mount, role)
			if err != nil {
				if log != nil {
					log.Warn("gatewayapi: renewing the vault token failed", "mount", mount, "role", role, "err", err)
				}
				timer.Reset(renewTokenRetryDelay(lease))
				continue
			}
			if auth.Lease <= 0 {
				return
			}
			lease = auth.Lease
			timer.Reset(renewTokenDelay(lease))
		}
	}
}

func renewTokenDelay(lease time.Duration) time.Duration {
	delay := max(lease*2/3, time.Second)
	return delay
}

func renewTokenRetryDelay(lease time.Duration) time.Duration {
	delay := 30 * time.Second
	if lease > 0 && lease/6 < delay {
		delay = lease / 6
	}
	if delay < time.Second {
		delay = time.Second
	}
	return delay
}

func custodyFromConfig(v VaultConfig) custodyEndpoint {
	return custodyEndpoint{
		Address:    v.Address,
		CACert:     append([]byte(nil), v.CACert...),
		ClientCert: append([]byte(nil), v.ClientCert...),
		ClientKey:  append([]byte(nil), v.ClientKey...),
		CertMount:  v.CertAuthMount,
		CertRole:   v.CertAuthRole,
		Timeout:    v.Timeout,
	}
}

func newOpenBaoClient(address, token string, ca, cert, key []byte) (*openbao.Client, error) {
	opts := []openbao.Option{}
	if len(ca) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("gatewayapi: the OpenBao CA bundle holds no certificate")
		}
		opts = append(opts, openbao.WithCACertPool(pool))
	}
	if len(cert) > 0 || len(key) > 0 {
		pair, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return nil, fmt.Errorf("gatewayapi: the OpenBao client certificate: %w", err)
		}
		opts = append(opts, openbao.WithClientCertificate(pair))
	}
	return openbao.New(address, token, opts...), nil
}

package componentcore

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	atidentity "github.com/bluesky-social/indigo/atproto/identity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	reporemote "github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/remote"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/repos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials"
)

const networkReadTimeout = 30 * time.Second

func (c *Core) repoOptions() repos.Options {
	cfg := c.cfg
	external := &http.Client{Timeout: networkReadTimeout}
	return repos.Options{
		Bus:     c.handlers,
		Log:     cfg.Log,
		Dynamic: atprotorecord.DynamicFactory(cfg.DynamicFactory),
		Network: reporemote.NewNetwork(reporemote.NetworkConfig{Directory: atidentity.DefaultDirectory(), HTTP: external}),
		Pds: func(workspace, name string) (*atpds.Server, bool) {
			svc, ok := c.registry.Service(workspace, "Pds", name)
			if !ok {
				return nil, false
			}
			server, ok := svc.(interface{ Server() *atpds.Server })
			if !ok {
				return nil, false
			}
			srv := server.Server()
			return srv, srv != nil
		},
		Secrets: func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error) {
			resolver, err := c.credentialResolver(workspace)
			if err != nil {
				return nil, err
			}
			credential, err := resolver.Resolve(ctx, credentialRef(ref))
			if err != nil {
				return nil, err
			}
			return credential.Data, nil
		},
	}
}

func (c *Core) credentialResolver(workspace string) (*credentials.Resolver, error) {
	if c.store == nil {
		return nil, errors.New("componentcore: no kcp store for the credential resolver")
	}
	return credentials.NewKCPResolver(c.store, workspace, c.cfg.CredentialClient, c.cfg.CredentialServer)
}

func credentialRef(ref commonv1.SecretRef) component.CredentialRef {
	return component.CredentialRef{Server: vaultServerIn(ref.Vault), Mount: strings.Trim(strings.TrimSpace(ref.Mount), "/"),
		Path: strings.Trim(strings.TrimSpace(ref.Path), "/")}
}

func vaultServerIn(r *commonv1.ObjectRef) string {
	server := v1alpha1.VaultServer(r, "")
	if server == "" || strings.EqualFold(strings.TrimSpace(r.Kind), v1alpha1.KindRemoteOpenBao) {
		return server
	}
	if ws := strings.TrimSpace(r.Workspace); ws != "" {
		return ws + "/" + server
	}
	return server
}

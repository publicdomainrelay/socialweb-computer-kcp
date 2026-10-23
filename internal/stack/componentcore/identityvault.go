package componentcore

import (
	"context"
	"errors"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/credentials"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type identityVaultStore struct {
	resolver *credentials.Resolver
}

func newIdentityVaultStore(store *kcpstore.Store, workspace, client, server string) (*identityVaultStore, error) {
	if strings.TrimSpace(client) == "" || strings.TrimSpace(server) == "" {
		return nil, errors.New("componentcore: the identity vault needs a credential client and server")
	}
	if store == nil {
		return nil, errors.New("componentcore: the identity vault needs the kcp store")
	}
	resolver, err := credentials.NewKCPResolver(store, workspace, client, server)
	if err != nil {
		return nil, err
	}
	return &identityVaultStore{resolver: resolver}, nil
}

func (s *identityVaultStore) ref(ref commonv1.SecretRef) component.CredentialRef {
	return component.CredentialRef{Mount: strings.Trim(strings.TrimSpace(ref.Mount), "/"), Path: strings.Trim(strings.TrimSpace(ref.Path), "/")}
}

func (s *identityVaultStore) Get(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
	credential, err := s.resolver.Resolve(ctx, s.ref(ref))
	if err != nil {
		if errors.Is(err, openbao.ErrNotFound) {
			return nil, atproto.ErrSecretNotFound
		}
		return nil, err
	}
	return credential.Data, nil
}

func (s *identityVaultStore) Put(ctx context.Context, ref commonv1.SecretRef, data map[string]any) error {
	return s.resolver.WriteCredential(ctx, s.ref(ref), data)
}

func (s *identityVaultStore) Delete(ctx context.Context, ref commonv1.SecretRef) error {
	return s.resolver.DeleteCredential(ctx, s.ref(ref))
}

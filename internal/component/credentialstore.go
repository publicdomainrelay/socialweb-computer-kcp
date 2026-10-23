package component

import (
	"context"
	"errors"
)

type CredentialStore struct {
	Resolver CredentialResolver

	Ref CredentialRef
}

func (s CredentialStore) Read(ctx context.Context, all bool) (map[string]any, error) {
	if s.Resolver == nil {
		return nil, errors.New("component: no credential resolver")
	}
	ref := s.Ref
	if all {
		ref.Keys = nil
	}
	credential, err := s.Resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	return credential.Data, nil
}

func (s CredentialStore) Write(ctx context.Context, data map[string]any) error {
	writer, ok := s.Resolver.(CredentialWriter)
	if !ok {
		return errors.New("component: the credential resolver cannot write")
	}
	return writer.WriteCredential(ctx, s.Ref, data)
}

func (s CredentialStore) Delete(ctx context.Context) error {
	deleter, ok := s.Resolver.(CredentialDeleter)
	if !ok {
		return nil
	}
	return deleter.DeleteCredential(ctx, s.Ref)
}

func (s CredentialStore) Writable() bool {
	_, ok := s.Resolver.(CredentialWriter)
	return ok
}

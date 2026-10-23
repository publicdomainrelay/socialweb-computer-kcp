package participant

import (
	"context"
	"errors"
	"maps"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

type Credentials interface {
	Read(ctx context.Context, all bool) (map[string]any, error)

	Write(ctx context.Context, data map[string]any) error

	Delete(ctx context.Context) error
}

type SessionStore struct {
	Credentials Credentials

	Merge bool
}

var _ oauthclient.SessionStore = (*SessionStore)(nil)

func (s *SessionStore) List(ctx context.Context) ([]oauthclient.SessionData, error) {
	if s == nil || s.Credentials == nil {
		return nil, errors.New("participant: no credentials hold the oauth session")
	}
	data, err := s.Credentials.Read(ctx, false)
	if err != nil {
		return nil, err
	}
	session, ok := oauthclient.SessionFromSecret(data)
	if !ok {
		return nil, nil
	}
	return []oauthclient.SessionData{session}, nil
}

func (s *SessionStore) Save(ctx context.Context, session oauthclient.SessionData) error {
	if s == nil || s.Credentials == nil {
		return errors.New("participant: no credentials hold the oauth session")
	}
	if session.AccountDID == "" {
		return errors.New("participant: the oauth session names no account DID")
	}
	data := map[string]any{}
	if s.Merge {
		if current, err := s.Credentials.Read(ctx, true); err == nil {
			maps.Copy(data, current)
		}
	}
	maps.Copy(data, oauthclient.SessionSecret(session))
	return s.Credentials.Write(ctx, data)
}

func (s *SessionStore) Delete(ctx context.Context, did, sessionID string) error {
	if s == nil || s.Credentials == nil {
		return nil
	}
	return s.Credentials.Delete(ctx)
}

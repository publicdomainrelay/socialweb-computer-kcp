package requester

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
)

type OAuthSession = oauthclient.Session

func openOAuthSession(ctx context.Context, cfg Config, dir identity.Directory) (*OAuthSession, error) {
	if dir == nil {
		dir = cfg.Directory
	}
	if cfg.OAuthSessionData != nil {
		data := *cfg.OAuthSessionData
		session, err := oauthclient.New(data, oauthclient.Options{
			DID:        data.AccountDID,
			RepoDID:    data.AccountDID,
			PDSURL:     data.HostURL,
			ClientID:   cfg.OAuthClientID,
			Scopes:     cfg.OAuthScopes,
			Directory:  dir,
			HTTPClient: cfg.HTTPClient,
		})
		if err != nil {
			return nil, fmt.Errorf("requester: the stored oauth session: %w", err)
		}
		return session, nil
	}
	if cfg.OAuthSessionPath == "" {
		return nil, errors.New("requester: oauth identity mode has no token store path")
	}
	session, err := oauthclient.OpenFile(ctx, cfg.OAuthSessionPath, oauthclient.Options{
		DID:        cfg.DID,
		Handle:     cfg.Handle,
		RepoDID:    cfg.RepoDID,
		PDSURL:     cfg.PDSURL,
		ClientID:   cfg.OAuthClientID,
		Scopes:     cfg.OAuthScopes,
		Directory:  dir,
		HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("requester: no oauth token store at %s", cfg.OAuthSessionPath)
		}
		return nil, err
	}
	return session, nil
}

package oauthclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

func BindEntrywayAccount(ctx context.Context, client *http.Client, directory identity.Directory, data SessionData) (SessionData, error) {
	entryway := strings.TrimRight(data.AuthServerURL, "/")
	if directory == nil {
		return SessionData{}, fmt.Errorf("oauthclient: no identity directory resolves the account the entryway %s signed in", entryway)
	}
	did, err := syntax.ParseDID(data.AccountDID)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the entryway account DID: %w", err)
	}
	ident, err := directory.LookupDID(ctx, did)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: resolving %s, the account the entryway %s signed in: %w", did, entryway, err)
	}
	pds := strings.TrimRight(strings.TrimSpace(ident.PDSEndpoint()), "/")
	if pds == "" {
		return SessionData{}, fmt.Errorf("oauthclient: %s names no PDS", did)
	}
	authServer, tokenEndpoint, revokeEndpoint, err := DiscoverAuthServer(ctx, client, pds)
	if err != nil {
		return SessionData{}, fmt.Errorf("oauthclient: the PDS %s of %s: %w", pds, did, err)
	}
	if authServer != entryway {
		return SessionData{}, fmt.Errorf("oauthclient: the PDS %s of %s names the authorization server %s, not the entryway %s", pds, did, authServer, entryway)
	}
	if err := checkAudience(data.AccessToken, pds); err != nil {
		return SessionData{}, err
	}
	data.HostURL = pds
	data.SessionID = SessionID(did.String(), pds)
	data.AuthServerURL = authServer
	data.AuthServerTokenEndpoint = tokenEndpoint
	data.AuthServerRevocationEndpoint = revokeEndpoint
	data.DPoPHostNonce = ""
	return data, nil
}

func checkAudience(token, pds string) error {
	audiences := tokenAudiences(token)
	host, err := url.Parse(pds)
	if err != nil || host.Host == "" {
		return fmt.Errorf("oauthclient: the PDS %q is not an absolute URL", pds)
	}
	checked := false
	for _, aud := range audiences {
		name, ok := strings.CutPrefix(aud, "did:web:")
		if !ok {
			continue
		}
		decoded, err := url.PathUnescape(name)
		if err != nil {
			continue
		}
		if strings.EqualFold(decoded, host.Host) {
			return nil
		}
		checked = true
	}
	if checked {
		return fmt.Errorf("oauthclient: the access token is for %s, not the PDS %s", strings.Join(audiences, ", "), pds)
	}
	return nil
}

func tokenAudiences(token string) []string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Audience json.RawMessage `json:"aud"`
	}
	if json.Unmarshal(raw, &claims) != nil || len(claims.Audience) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(claims.Audience, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(claims.Audience, &many) == nil {
		return many
	}
	return nil
}

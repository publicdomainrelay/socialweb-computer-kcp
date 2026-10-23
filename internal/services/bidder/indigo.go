package bidder

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/auth/oauth"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

func indigoSessionPDS(data oauthclient.SessionData) (pdsclient.PDS, error) {
	key, err := atcrypto.ParsePrivateMultibase(data.DPoPPrivateKeyMultibase)
	if err != nil {
		return nil, err
	}
	scopes := append([]string(nil), data.Scopes...)
	if !slices.Contains(scopes, "atproto") {
		scopes = append([]string{"atproto"}, scopes...)
	}
	config := oauth.NewPublicConfig("https://socialweb.computer/oauth-client-metadata.json", "", scopes)
	session := &oauth.ClientSession{
		Client:         &http.Client{Timeout: 30 * time.Second},
		Config:         &config,
		DPoPPrivateKey: key,
		Data: &oauth.ClientSessionData{
			AccountDID:                   syntax.DID(data.AccountDID),
			SessionID:                    data.SessionID,
			HostURL:                      data.HostURL,
			AuthServerURL:                data.AuthServerURL,
			AuthServerTokenEndpoint:      data.AuthServerTokenEndpoint,
			AuthServerRevocationEndpoint: data.AuthServerRevocationEndpoint,
			Scopes:                       scopes,
			AccessToken:                  data.AccessToken,
			RefreshToken:                 data.RefreshToken,
			DPoPAuthServerNonce:          data.DPoPAuthServerNonce,
			DPoPHostNonce:                data.DPoPHostNonce,
			DPoPPrivateKeyMultibase:      data.DPoPPrivateKeyMultibase,
		},
	}
	api := atclient.NewAPIClient(strings.TrimRight(data.HostURL, "/"))
	api.Client = &http.Client{Timeout: 30 * time.Second}
	api.Auth = session
	return &pdsclient.IndigoPDS{API: api, BaseURL: data.HostURL}, nil
}

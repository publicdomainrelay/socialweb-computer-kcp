package oauthclient

import (
	"encoding/json"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

func SessionFromSecret(data map[string]any) (SessionData, bool) {
	if raw := strs.Lookup(data, "session"); raw != "" {
		var session SessionData
		if err := json.Unmarshal([]byte(raw), &session); err != nil {
			return SessionData{}, false
		}
		if session.AccountDID != "" && session.HostURL != "" {
			return session, true
		}
	}
	session := SessionData{
		AccountDID:                   strs.Lookup(data, "account_did", "did", "userDid"),
		SessionID:                    strs.Lookup(data, "session_id"),
		HostURL:                      strs.Lookup(data, "host_url", "pds"),
		ClientID:                     strs.Lookup(data, "client_id", "clientId"),
		AuthServerURL:                strs.Lookup(data, "authserver_url"),
		AuthServerTokenEndpoint:      strs.Lookup(data, "authserver_token_endpoint"),
		AuthServerRevocationEndpoint: strs.Lookup(data, "authserver_revocation_endpoint"),
		Scopes:                       strings.Fields(strs.Lookup(data, "scopes", "scope")),
		AccessToken:                  strs.Lookup(data, "access_token", "accessJwt", "accessToken"),
		RefreshToken:                 strs.Lookup(data, "refresh_token", "refreshJwt", "refreshToken"),
		DPoPAuthServerNonce:          strs.Lookup(data, "dpop_authserver_nonce"),
		DPoPHostNonce:                strs.Lookup(data, "dpop_host_nonce"),
		DPoPPrivateKeyMultibase:      strs.Lookup(data, "dpop_privatekey_multibase", "dpopPrivateKeyMultibase"),
	}
	if session.AccountDID == "" || session.HostURL == "" || session.AccessToken == "" {
		return SessionData{}, false
	}
	return session, true
}

func SessionSecret(session SessionData) map[string]any {
	raw, _ := json.Marshal(session)
	return map[string]any{
		"session":                   string(raw),
		"account_did":               session.AccountDID,
		"session_id":                session.SessionID,
		"did":                       session.AccountDID,
		"host_url":                  session.HostURL,
		"pds":                       session.HostURL,
		"client_id":                 session.ClientID,
		"authserver_url":            session.AuthServerURL,
		"authserver_token_endpoint": session.AuthServerTokenEndpoint,
		"scopes":                    strings.Join(session.Scopes, " "),
		"access_token":              session.AccessToken,
		"refresh_token":             session.RefreshToken,
		"accessJwt":                 session.AccessToken,
		"refreshJwt":                session.RefreshToken,
		"dpop_authserver_nonce":     session.DPoPAuthServerNonce,
		"dpop_host_nonce":           session.DPoPHostNonce,
		"dpop_privatekey_multibase": session.DPoPPrivateKeyMultibase,
		"dpopPrivateKeyMultibase":   session.DPoPPrivateKeyMultibase,
	}
}

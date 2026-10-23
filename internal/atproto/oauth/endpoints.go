package oauth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const requestURIPrefix = "urn:ietf:params:oauth:request_uri:"

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Sub          string `json:"sub,omitempty"`
}

func (s *Server) verifyClientAssertion(ctx context.Context, form url.Values, now time.Time) (string, string, error) {
	assertion := form.Get("client_assertion")
	if assertion == "" {
		if form.Get("client_assertion_type") != "" {
			return "", "", fmt.Errorf("%w: client_assertion is required", ErrInvalidClient)
		}
		return "", "", nil
	}
	if form.Get("client_assertion_type") != ClientAssertionTypeJWTBearer {
		return "", "", fmt.Errorf("%w: unsupported client assertion type", ErrInvalidClient)
	}
	clientID := form.Get("client_id")
	if clientID == "" {
		return "", "", fmt.Errorf("%w: client_id is required with client assertions", ErrInvalidClient)
	}
	_, keyJKT, err := s.clients.VerifyClientAssertion(ctx, assertion, clientID, s.issuer, now)
	if err != nil {
		return "", "", err
	}
	return clientID, keyJKT, nil
}

func (s *Server) handlePAR(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.verifyAuthorizationDPoP(w, r, ""); !ok {
		return
	}
	form, err := parseOAuthForm(w, r)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	now := time.Now()
	ctx := r.Context()

	clientID := form.Get("client_id")
	redirectURI := form.Get("redirect_uri")
	responseType := form.Get("response_type")
	state := form.Get("state")
	challenge := form.Get("code_challenge")
	method := form.Get("code_challenge_method")
	method = cmp.Or(method, "S256")
	if clientID == "" || redirectURI == "" || responseType == "" || state == "" || challenge == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id, code_challenge, redirect_uri, response_type, and state required")
		return
	}
	if responseType != "code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "")
		return
	}
	if err := validateCodeChallenge(challenge, method); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	metadata, err := s.clients.Resolve(ctx, clientID)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "could not resolve client metadata")
		return
	}
	if !RedirectURIAllowed(metadata, clientID, redirectURI) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not registered in client metadata")
		return
	}
	scope, err := normalizeScope(form.Get("scope"), metadata.Scope)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", err.Error())
		return
	}
	assertedClient, assertedKey, err := s.verifyClientAssertion(ctx, form, now)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	authMethod := metadata.TokenEndpointAuthMethod
	if assertedClient != "" {
		authMethod = ClientAuthPrivateKeyJWT
	}
	if metadata.TokenEndpointAuthMethod == ClientAuthPrivateKeyJWT && assertedClient == "" {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client assertion required")
		return
	}

	requestID, err := randomToken(32)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	request := PARRequest{
		RequestURI:          requestURIPrefix + requestID,
		ClientID:            clientID,
		ClientAuthMethod:    authMethod,
		ClientKeyJKT:        assertedKey,
		RedirectURI:         redirectURI,
		Scope:               scope,
		State:               state,
		CodeChallenge:       challenge,
		CodeChallengeMethod: method,
		ResponseType:        responseType,
		CreatedAt:           now,
		ExpiresAt:           now.Add(s.config.PARTTL),
	}
	if err := s.pars.StorePAR(ctx, request); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpjson.Write(w, http.StatusOK, struct {
		RequestURI string `json:"request_uri"`
		ExpiresIn  int64  `json:"expires_in"`
	}{RequestURI: request.RequestURI, ExpiresIn: int64(s.config.PARTTL / time.Second)})

}

func (s *Server) handleAuthorizePage(w http.ResponseWriter, r *http.Request) {
	requestURI := r.URL.Query().Get("request_uri")
	clientID := r.URL.Query().Get("client_id")
	if requestURI == "" || clientID == "" {
		http.Error(w, "request_uri and client_id are required", http.StatusBadRequest)
		return
	}
	handoff, err := json.Marshal(map[string]string{"requestUri": requestURI, "clientId": clientID})
	if err != nil {
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, authorizePage, handoff)
}

const authorizePage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>Sign in</title></head>
<body>
<main>
<h1>Sign in</h1>
<form id="signin">
<p><input type="text" id="identifier" name="identifier" placeholder="handle or email" autocomplete="username" required></p>
<p><input type="password" id="password" name="password" placeholder="password" autocomplete="current-password" required></p>
<p><button type="submit" id="submit">Sign in</button></p>
<p id="error" role="alert"></p>
</form>
</main>
<script>
const handoff = %s;
document.getElementById("signin").addEventListener("submit", async (event) => {
  event.preventDefault();
  const body = new URLSearchParams({
    identifier: document.getElementById("identifier").value,
    password: document.getElementById("password").value,
    request_uri: handoff.requestUri,
    client_id: handoff.clientId,
  });
  const response = await fetch(window.location.pathname, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });
  const data = await response.json().catch(() => ({}));
  if (!response.ok || !data.redirect_uri) {
    document.getElementById("error").textContent = data.error_description || data.error || ("HTTP " + response.status);
    return;
  }
  window.location.href = data.redirect_uri;
});
</script>
</body>
</html>`

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	form, err := parseOAuthForm(w, r)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	now := time.Now()
	ctx := r.Context()

	identifier := form.Get("identifier")
	password := form.Get("password")
	var account Account
	if identifier != "" || password != "" {
		if identifier == "" || password == "" {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid identifier or password")
			return
		}
		authenticated, err := s.accounts.Authenticate(ctx, identifier, password)
		if err != nil {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_grant", "invalid identifier or password")
			return
		}
		account = authenticated
	} else {
		scheme, token, ok := serviceauth.SplitAuthorization(r.Header.Get("Authorization"))
		if !ok || !strings.EqualFold(scheme, AuthSchemeDPoP) {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_request", "DPoP authorization required")
			return
		}
		session, err := s.ValidateAccessToken(ctx, token)
		if err != nil {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_token", "")
			return
		}
		account = session.Account
	}

	requestURI := form.Get("request_uri")
	var request PARRequest
	if requestURI != "" {
		clientID := form.Get("client_id")
		if clientID == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id is required")
			return
		}
		stored, err := s.pars.ConsumePAR(ctx, requestURI, now)
		if err != nil || stored == nil || !stored.ExpiresAt.After(now) || stored.ClientID != clientID {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid or expired request_uri")
			return
		}
		request = *stored
	} else {
		clientID := form.Get("client_id")
		redirectURI := form.Get("redirect_uri")
		challenge := form.Get("code_challenge")
		method := form.Get("code_challenge_method")
		method = cmp.Or(method, "S256")
		responseType := form.Get("response_type")
		if clientID == "" || redirectURI == "" || challenge == "" {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code_challenge and redirect_uri required")
			return
		}
		if responseType != "" && responseType != "code" {
			writeOAuthError(w, http.StatusBadRequest, "unsupported_response_type", "")
			return
		}
		if err := validateCodeChallenge(challenge, method); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		metadata, err := s.clients.Resolve(ctx, clientID)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "could not resolve client metadata")
			return
		}
		if !RedirectURIAllowed(metadata, clientID, redirectURI) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri not registered in client metadata")
			return
		}
		scope, err := normalizeScope(form.Get("scope"), metadata.Scope)
		if err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_scope", err.Error())
			return
		}
		responseType = cmp.Or(responseType, "code")
		request = PARRequest{
			ClientID:            clientID,
			ClientAuthMethod:    metadata.TokenEndpointAuthMethod,
			RedirectURI:         redirectURI,
			Scope:               scope,
			State:               form.Get("state"),
			CodeChallenge:       challenge,
			CodeChallengeMethod: method,
			ResponseType:        responseType,
			CreatedAt:           now,
			ExpiresAt:           now.Add(s.config.AuthorizationCodeTTL),
		}
	}

	codeID, err := randomToken(32)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	sessionID, err := randomToken(32)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	code := AuthorizationCode{
		ID:                  codeID,
		SessionID:           sessionID,
		ClientID:            request.ClientID,
		ClientAuthMethod:    request.ClientAuthMethod,
		ClientKeyJKT:        request.ClientKeyJKT,
		Account:             account,
		Scope:               request.Scope,
		RedirectURI:         request.RedirectURI,
		CodeChallenge:       request.CodeChallenge,
		CodeChallengeMethod: request.CodeChallengeMethod,
		CreatedAt:           now,
		ExpiresAt:           now.Add(s.config.AuthorizationCodeTTL),
	}
	if err := s.codes.CreateCode(ctx, code); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	redirect, err := authorizeRedirect(request.RedirectURI, codeID, request.State, s.issuer)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpjson.Write(w, http.StatusOK, struct {
		RedirectURI string `json:"redirect_uri"`
		Code        string `json:"code"`
	}{RedirectURI: redirect, Code: codeID})

}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	proof, ok := s.verifyAuthorizationDPoP(w, r, "")
	if !ok {
		return
	}
	form, err := parseOAuthForm(w, r)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	now := time.Now()
	ctx := r.Context()

	assertedClient, assertedKey, err := s.verifyClientAssertion(ctx, form, now)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "client assertion verification failed")
		return
	}
	formClient := form.Get("client_id")
	if assertedClient != "" {
		formClient = cmp.Or(formClient, assertedClient)
		if formClient != assertedClient {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
			return
		}
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		s.handleAuthorizationCodeGrant(w, r, form, proof, formClient, assertedClient, assertedKey, now)
	case "refresh_token":
		s.handleRefreshGrant(w, r, form, proof, formClient, assertedClient, assertedKey, now)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

func (s *Server) handleAuthorizationCodeGrant(w http.ResponseWriter, r *http.Request, form url.Values, proof *DPoPProof, formClient, assertedClient, assertedKey string, now time.Time) {
	ctx := r.Context()
	codeID := form.Get("code")
	verifier := form.Get("code_verifier")
	if codeID == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier required")
		return
	}
	code, err := s.codes.ConsumeCode(ctx, codeID, now)
	if err != nil {
		if errors.Is(err, ErrCodeUsed) && code != nil {
			s.revokeSession(ctx, code.SessionID, now)
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if code == nil || !code.ExpiresAt.After(now) || code.CodeChallengeMethod != "S256" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
		return
	}
	if !verifyPKCE(verifier, code.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid code_verifier")
		return
	}
	redirectURI := form.Get("redirect_uri")
	if redirectURI == "" || redirectURI != code.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
		return
	}
	if formClient != "" && formClient != code.ClientID {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if code.ClientAuthMethod == ClientAuthPrivateKeyJWT {
		if assertedClient == "" || assertedClient != code.ClientID {
			writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
			return
		}
	}
	if code.ClientKeyJKT != "" && assertedKey != code.ClientKeyJKT {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	session := Session{
		ID:               code.SessionID,
		Account:          code.Account,
		ClientID:         code.ClientID,
		ClientAuthMethod: code.ClientAuthMethod,
		ClientKeyJKT:     code.ClientKeyJKT,
		Scope:            code.Scope,
		JKT:              proof.JKT,
		CreatedAt:        now,
		ExpiresAt:        now.Add(s.config.RefreshTokenTTL),
	}
	if err := s.sessions.CreateSession(ctx, session); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	access, refresh, err := s.mintTokenPair(session, now)
	if err != nil {
		s.revokeSession(ctx, session.ID, now)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if err := s.storeTokenPair(ctx, session.ID, access, refresh); err != nil {
		s.revokeSession(ctx, session.ID, now)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpjson.Write(w, http.StatusOK, tokenResponse{
		AccessToken:  access.Token,
		TokenType:    AuthSchemeDPoP,
		RefreshToken: refresh.Token,
		ExpiresIn:    int64(s.config.AccessTokenTTL / time.Second),
		Scope:        session.Scope,
		Sub:          session.Account.DID.String(),
	})

}

func (s *Server) handleRefreshGrant(w http.ResponseWriter, r *http.Request, form url.Values, proof *DPoPProof, formClient, assertedClient, assertedKey string, now time.Time) {
	ctx := r.Context()
	refreshToken := form.Get("refresh_token")
	if refreshToken == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "missing refresh_token")
		return
	}
	claims, err := parseOAuthToken(refreshToken, TokenRefresh, s.signer, now)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		return
	}
	record, err := s.tokens.GetToken(ctx, claims.JTI)
	if err != nil || record == nil || record.Kind != TokenRefresh || record.RevokedAt != nil || !record.ExpiresAt.After(now) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		return
	}
	session, err := s.sessions.GetSession(ctx, record.SessionID)
	if err != nil || session == nil || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		return
	}
	if session.ID != record.SessionID || claims.Sub != session.Account.DID.String() || claims.CNF.JKT != session.JKT || proof.JKT != session.JKT {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is not bound to this DPoP key")
		return
	}
	if formClient != "" && formClient != session.ClientID {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if assertedClient != "" && assertedClient != session.ClientID {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if session.ClientAuthMethod == ClientAuthPrivateKeyJWT && assertedClient != session.ClientID {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	if session.ClientKeyJKT != "" && assertedKey != session.ClientKeyJKT {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "")
		return
	}
	access, refresh, err := s.mintTokenPair(*session, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if err := s.sessions.ExtendSession(ctx, session.ID, now.Add(s.config.RefreshTokenTTL)); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	records := []TokenRecord{
		makeTokenRecord(session.ID, TokenAccess, access),
		makeTokenRecord(session.ID, TokenRefresh, refresh),
	}
	if err := s.tokens.RotateRefreshToken(ctx, claims.JTI, now, records); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	httpjson.Write(w, http.StatusOK, tokenResponse{
		AccessToken:  access.Token,
		TokenType:    AuthSchemeDPoP,
		RefreshToken: refresh.Token,
		ExpiresIn:    int64(s.config.AccessTokenTTL / time.Second),
		Scope:        session.Scope,
	})

}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(HeaderDPoP) != "" {
		if _, ok := s.verifyAuthorizationDPoP(w, r, ""); !ok {
			return
		}
	}
	form, err := parseOAuthForm(w, r)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	token := form.Get("token")
	if token == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token required")
		return
	}
	now := time.Now()
	ctx := r.Context()

	claims, kind, ok := parseRevocableToken(token, s.signer, now)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		httpjson.Write(w, http.StatusOK, map[string]any{})
		return
	}
	record, err := s.tokens.GetToken(ctx, claims.JTI)
	if err == nil && record != nil {
		if kind == TokenRefresh {
			s.revokeSession(ctx, record.SessionID, now)
		} else {
			_ = s.tokens.RevokeToken(ctx, record.JTI, now)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, map[string]any{})
}

func parseRevocableToken(token string, signer Signer, now time.Time) (*tokenClaims, TokenKind, bool) {
	if claims, err := parseOAuthToken(token, TokenAccess, signer, now); err == nil {
		return claims, TokenAccess, true
	}
	if claims, err := parseOAuthToken(token, TokenRefresh, signer, now); err == nil {
		return claims, TokenRefresh, true
	}
	return nil, "", false
}

func (s *Server) revokeSession(ctx context.Context, sessionID string, at time.Time) {
	if sessionID == "" {
		return
	}
	_ = s.sessions.RevokeSession(ctx, sessionID, at)
	_ = s.tokens.RevokeSessionTokens(ctx, sessionID, at)
}

func (s *Server) mintTokenPair(session Session, now time.Time) (*signedToken, *signedToken, error) {
	access, err := issueSignedToken(s.signer, now, TokenAccess, session, s.config.AccessTokenTTL)
	if err != nil {
		return nil, nil, err
	}
	refresh, err := issueSignedToken(s.signer, now, TokenRefresh, session, s.config.RefreshTokenTTL)
	if err != nil {
		return nil, nil, err
	}
	return access, refresh, nil
}

func (s *Server) storeTokenPair(ctx context.Context, sessionID string, access, refresh *signedToken) error {
	if err := s.tokens.CreateToken(ctx, makeTokenRecord(sessionID, TokenAccess, access)); err != nil {
		return err
	}
	if err := s.tokens.CreateToken(ctx, makeTokenRecord(sessionID, TokenRefresh, refresh)); err != nil {
		_ = s.tokens.RevokeToken(ctx, access.JTI, time.Now())
		return err
	}
	return nil
}

func makeTokenRecord(sessionID string, kind TokenKind, token *signedToken) TokenRecord {
	return TokenRecord{
		JTI:       token.JTI,
		SessionID: sessionID,
		Kind:      kind,
		IssuedAt:  token.IssuedAt,
		ExpiresAt: token.ExpiresAt,
	}
}

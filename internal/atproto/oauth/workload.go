package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const (
	NSIDExchangeToken = "com.fedproxy.temp.oauth.exchangeToken"

	NSIDApproveAuthorization = "com.fedproxy.temp.oauth.approveAuthorization"

	PathExchangeToken = "/xrpc/" + NSIDExchangeToken

	PathApproveAuthorization = "/xrpc/" + NSIDApproveAuthorization

	TokenTypeWorkload = "wid+jwt"

	TokenTypeJWT = "urn:ietf:params:oauth:token-type:jwt"

	TokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"

	WorkloadRoleFragment = "role/"

	DefaultWorkloadTokenTTL = 5 * time.Minute
)

type WorkloadGrant struct {
	SessionID string

	Grant workloadauth.Grant

	ClientID string

	RedirectURI string

	Scope string

	CreatedAt time.Time
}

type WorkloadGrantStore interface {
	PutWorkloadGrant(ctx context.Context, grant WorkloadGrant) error
	GetWorkloadGrant(ctx context.Context, sessionID string) (*WorkloadGrant, error)
	WorkloadGrantSessions(ctx context.Context, account, role string) ([]string, error)
}

type WorkloadAccounts interface {
	ActiveAccount(ctx context.Context, did syntax.DID) (Account, error)
}

type WorkloadConfig struct {
	Authorizer workloadauth.Authorizer

	Grants WorkloadGrantStore

	Accounts WorkloadAccounts

	TokenTTL time.Duration

	Log *slog.Logger
}

type workloadActor struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
}

type workloadClaims struct {
	Iss string        `json:"iss"`
	Sub string        `json:"sub"`
	Aud string        `json:"aud"`
	Lxm string        `json:"lxm"`
	Act workloadActor `json:"act"`
	CNF tokenCNF      `json:"cnf"`
	Iat int64         `json:"iat"`
	Exp int64         `json:"exp"`
	JTI string        `json:"jti"`
}

type exchangeTokenInput struct {
	SubjectToken       string `json:"subject_token"`
	SubjectTokenType   string `json:"subject_token_type"`
	Audience           string `json:"audience"`
	RequestedTokenType string `json:"requested_token_type,omitempty"`
}

type exchangeTokenOutput struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

type approveAuthorizationInput struct {
	RequestURI string `json:"request_uri"`
}

type approveAuthorizationOutput struct {
	RedirectURI string `json:"redirect_uri"`
}

func WorkloadAudience(account, role string) string {
	return account + "#" + WorkloadRoleFragment + role
}

func ParseWorkloadAudience(aud string) (syntax.DID, string, error) {
	head, fragment, ok := strings.Cut(strings.TrimSpace(aud), "#")
	if !ok {
		return "", "", fmt.Errorf("oauth: audience %q is not <did>#role/<rkey>", aud)
	}
	did, err := syntax.ParseDID(head)
	if err != nil {
		return "", "", fmt.Errorf("oauth: audience %q names no DID: %w", aud, err)
	}
	role, ok := strings.CutPrefix(fragment, WorkloadRoleFragment)
	if !ok || role == "" {
		return "", "", fmt.Errorf("oauth: audience %q names no role", aud)
	}
	if _, err := syntax.ParseRecordKey(role); err != nil {
		return "", "", fmt.Errorf("oauth: audience %q names a role that is not a record key: %w", aud, err)
	}
	return did, role, nil
}

func ApprovalDocument(clientID, redirectURI, scope string) map[string]any {
	scopes := strings.Fields(scope)
	if scopes == nil {
		scopes = []string{}
	}
	return map[string]any{"client_id": clientID, "redirect_uri": redirectURI, "scope": scopes}
}

func (s *Server) workloadLog() *slog.Logger {
	if s.workload != nil && s.workload.Log != nil {
		return s.workload.Log
	}
	return slog.New(slog.DiscardHandler)
}

func writeXRPCError(w http.ResponseWriter, status int, code, message string) {
	httpjson.Write(w, status, map[string]string{"error": code, "message": message})
}

func (s *Server) registerWorkload(mux *http.ServeMux) {
	if s.workload == nil {
		return
	}
	mux.HandleFunc("POST "+PathExchangeToken, s.handleExchangeToken)
	mux.HandleFunc("POST "+PathApproveAuthorization, s.handleApproveAuthorization)
}

func (s *Server) verifyResourceDPoP(w http.ResponseWriter, r *http.Request, accessToken string) (*DPoPProof, bool) {
	ctx := r.Context()
	requestURL := s.resourceOrigin(r) + r.URL.EscapedPath()
	nonceOrigin, _ := dpopOrigin(requestURL)
	nonce, err := s.nonces.IssueNonce(ctx, nonceOrigin)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "")
		return nil, false
	}
	w.Header().Set(HeaderDPoPNonce, nonce)
	proof, err := s.dpop.Verify(ctx, r.Header.Get(HeaderDPoP), r.Method, requestURL, accessToken)
	if err != nil {
		if errors.Is(err, ErrUseDPoPNonce) {
			writeResourceError(w, http.StatusUnauthorized, "use_dpop_nonce", "", nonce)
			return nil, false
		}
		writeResourceError(w, http.StatusUnauthorized, "invalid_dpop_proof", "", nonce)
		return nil, false
	}
	return proof, true
}

func decodeWorkloadInput(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxOAuthFormBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON input")
		return false
	}
	return true
}

func (s *Server) handleExchangeToken(w http.ResponseWriter, r *http.Request) {
	proof, ok := s.verifyResourceDPoP(w, r, "")
	if !ok {
		return
	}
	var in exchangeTokenInput
	if !decodeWorkloadInput(w, r, &in) {
		return
	}
	ctx := r.Context()
	log := s.workloadLog()
	if in.SubjectToken == "" || in.SubjectTokenType != TokenTypeJWT {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "subject_token of type "+TokenTypeJWT+" is required")
		return
	}
	if in.RequestedTokenType != "" && in.RequestedTokenType != TokenTypeAccessToken {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "requested_token_type must be "+TokenTypeAccessToken)
		return
	}
	did, role, err := ParseWorkloadAudience(in.Audience)
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	if _, err := s.workload.Accounts.ActiveAccount(ctx, did); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "the audience names no active account on this PDS")
		return
	}
	grant, err := s.workload.Authorizer.Admit(ctx, did.String(), role, in.SubjectToken,
		workloadauth.Action{Path: PathExchangeToken, Method: http.MethodPost})
	if err != nil {
		log.Warn("pds_workload_exchange_refused", "account", did.String(), "role", role, "err", err.Error())
		writeXRPCError(w, http.StatusForbidden, "Forbidden", err.Error())
		return
	}
	if grant.Audience != in.Audience {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "the role's audience is not the requested audience")
		return
	}
	now := time.Now()
	if grant.NotAfter.IsZero() || !grant.NotAfter.After(now) {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "the workload token has no future expiry")
		return
	}
	if grant.TokenID != "" {
		fresh, err := s.nonces.RecordJTI(ctx, "wid-subject\x00"+grant.Issuer+"\x00"+grant.TokenID, grant.NotAfter)
		if err != nil || !fresh {
			writeXRPCError(w, http.StatusForbidden, "Forbidden", "the workload token was already exchanged")
			return
		}
	}
	ttl := s.workload.TokenTTL
	if ttl <= 0 {
		ttl = DefaultWorkloadTokenTTL
	}
	exp := now.Add(ttl)
	if grant.NotAfter.Before(exp) {
		exp = grant.NotAfter
	}
	token, err := s.issueWorkloadToken(grant, proof.JKT, now, exp)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "")
		return
	}
	log.Info("pds_workload_exchanged", "account", did.String(), "role", role, "issuer", grant.Issuer, "subject", grant.Subject)
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, exchangeTokenOutput{
		AccessToken:     token,
		IssuedTokenType: TokenTypeAccessToken,
		TokenType:       AuthSchemeDPoP,
		ExpiresIn:       int64(exp.Sub(now) / time.Second),
	})
}

func (s *Server) issueWorkloadToken(grant workloadauth.Grant, jkt string, now, exp time.Time) (string, error) {
	alg, err := signingAlgorithm(s.signer.Key)
	if err != nil {
		return "", err
	}
	jti, err := randomToken(16)
	if err != nil {
		return "", err
	}
	header := tokenHeader{Typ: TokenTypeWorkload, Alg: alg, Kid: s.signer.DID.String() + "#atproto"}
	claims := workloadClaims{
		Iss: s.signer.DID.String(),
		Sub: grant.Account,
		Aud: grant.Audience,
		Lxm: NSIDApproveAuthorization,
		Act: workloadActor{Iss: grant.Issuer, Sub: grant.Subject},
		CNF: tokenCNF{JKT: jkt},
		Iat: now.Unix(),
		Exp: exp.Unix(),
		JTI: jti,
	}
	headerPart, err := encodeSegment(header)
	if err != nil {
		return "", err
	}
	payloadPart, err := encodeSegment(claims)
	if err != nil {
		return "", err
	}
	input := headerPart + "." + payloadPart
	signature, err := s.signer.Key.HashAndSign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *Server) parseWorkloadToken(token string, now time.Time) (*workloadClaims, error) {
	headerPart, payloadPart, signaturePart, ok := splitJWT(token)
	if !ok {
		return nil, fmt.Errorf("%w: malformed JWT", ErrInvalidToken)
	}
	var header tokenHeader
	if err := decodeSegment(headerPart, &header); err != nil || header.Typ != TokenTypeWorkload {
		return nil, fmt.Errorf("%w: not a workload token", ErrInvalidToken)
	}
	alg, err := signingAlgorithm(s.signer.Key)
	if err != nil {
		return nil, err
	}
	if header.Alg != alg {
		return nil, fmt.Errorf("%w: unexpected alg", ErrInvalidToken)
	}
	signature, err := base64.RawURLEncoding.DecodeString(signaturePart)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed signature", ErrInvalidToken)
	}
	publicKey, err := s.signer.Key.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if err := publicKey.HashAndVerifyLenient([]byte(headerPart+"."+payloadPart), signature); err != nil {
		return nil, fmt.Errorf("%w: bad signature", ErrInvalidToken)
	}
	var claims workloadClaims
	if err := decodeSegment(payloadPart, &claims); err != nil {
		return nil, fmt.Errorf("%w: malformed claims", ErrInvalidToken)
	}
	switch {
	case claims.Iss != s.signer.DID.String():
		return nil, fmt.Errorf("%w: unexpected issuer", ErrInvalidToken)
	case claims.Lxm != NSIDApproveAuthorization:
		return nil, fmt.Errorf("%w: the token is not for %s", ErrInvalidToken, NSIDApproveAuthorization)
	case claims.JTI == "" || claims.Exp == 0 || claims.Iat == 0 || claims.CNF.JKT == "":
		return nil, fmt.Errorf("%w: missing claims", ErrInvalidToken)
	case now.Unix() >= claims.Exp:
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	case claims.Act.Iss == "" || claims.Act.Sub == "":
		return nil, fmt.Errorf("%w: the token names no actor", ErrInvalidToken)
	}
	return &claims, nil
}

func (s *Server) handleApproveAuthorization(w http.ResponseWriter, r *http.Request) {
	scheme, token, ok := serviceauth.SplitAuthorization(r.Header.Get("Authorization"))
	if !ok || !strings.EqualFold(scheme, AuthSchemeDPoP) || token == "" {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "a DPoP workload token is required")
		return
	}
	proof, ok := s.verifyResourceDPoP(w, r, token)
	if !ok {
		return
	}
	ctx := r.Context()
	log := s.workloadLog()
	now := time.Now()
	claims, err := s.parseWorkloadToken(token, now)
	if err != nil {
		writeXRPCError(w, http.StatusUnauthorized, "InvalidToken", err.Error())
		return
	}
	if claims.CNF.JKT != proof.JKT {
		writeXRPCError(w, http.StatusUnauthorized, "InvalidToken", "the token is not bound to this DPoP key")
		return
	}
	did, role, err := ParseWorkloadAudience(claims.Aud)
	if err != nil || did.String() != claims.Sub {
		writeXRPCError(w, http.StatusUnauthorized, "InvalidToken", "the token's audience is not its account")
		return
	}
	fresh, err := s.nonces.RecordJTI(ctx, "wid-token\x00"+claims.JTI, time.Unix(claims.Exp, 0))
	if err != nil || !fresh {
		writeXRPCError(w, http.StatusUnauthorized, "InvalidToken", "the workload token was already used")
		return
	}
	var in approveAuthorizationInput
	if !decodeWorkloadInput(w, r, &in) {
		return
	}
	if in.RequestURI == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "request_uri is required")
		return
	}
	pending, err := s.pars.GetPAR(ctx, in.RequestURI, now)
	if err != nil || pending == nil || !pending.ExpiresAt.After(now) {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid or expired request_uri")
		return
	}
	grant := workloadauth.Grant{Account: did.String(), Role: role, Issuer: claims.Act.Iss, Subject: claims.Act.Sub, Audience: claims.Aud}
	doc := ApprovalDocument(pending.ClientID, pending.RedirectURI, pending.Scope)
	if err := s.workload.Authorizer.Check(ctx, grant, workloadauth.Action{Path: PathApproveAuthorization, Method: http.MethodPost, Body: doc}); err != nil {
		log.Warn("pds_workload_approve_refused", "account", did.String(), "role", role, "client", pending.ClientID, "err", err.Error())
		writeXRPCError(w, http.StatusForbidden, "Forbidden", err.Error())
		return
	}
	account, err := s.workload.Accounts.ActiveAccount(ctx, did)
	if err != nil {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "the account is not active")
		return
	}
	request, err := s.pars.ConsumePAR(ctx, in.RequestURI, now)
	if err != nil || request == nil || request.ClientID != pending.ClientID || request.Scope != pending.Scope || request.RedirectURI != pending.RedirectURI {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid or expired request_uri")
		return
	}
	sessionID, err := randomToken(32)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "")
		return
	}
	if err := s.workload.Grants.PutWorkloadGrant(ctx, WorkloadGrant{
		SessionID: sessionID, Grant: grant, ClientID: request.ClientID, RedirectURI: request.RedirectURI,
		Scope: request.Scope, CreatedAt: now,
	}); err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "")
		return
	}
	redirect, _, err := s.issueCode(ctx, *request, account, sessionID, now)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "")
		return
	}
	log.Info("pds_workload_approved", "account", did.String(), "role", role, "client", request.ClientID, "scope", request.Scope)
	w.Header().Set("Cache-Control", "no-store")
	httpjson.Write(w, http.StatusOK, approveAuthorizationOutput{RedirectURI: redirect})
}

func (s *Server) workloadSessionAllowed(ctx context.Context, sessionID string) bool {
	if s.workload == nil {
		return true
	}
	grant, err := s.workload.Grants.GetWorkloadGrant(ctx, sessionID)
	if err != nil {
		return false
	}
	if grant == nil {
		return true
	}
	err = s.workload.Authorizer.Check(ctx, grant.Grant, workloadauth.Action{
		Path: PathApproveAuthorization, Method: http.MethodPost,
		Body: ApprovalDocument(grant.ClientID, grant.RedirectURI, grant.Scope),
	})
	if err != nil {
		s.workloadLog().Info("pds_workload_session_refused", "account", grant.Grant.Account, "role", grant.Grant.Role, "err", err.Error())
		return false
	}
	return true
}

func (s *Server) RecheckWorkloadGrants(ctx context.Context, account, role string) {
	if s.workload == nil {
		return
	}
	sessions, err := s.workload.Grants.WorkloadGrantSessions(ctx, account, role)
	if err != nil {
		s.workloadLog().Warn("pds_workload_recheck_failed", "account", account, "role", role, "err", err.Error())
		return
	}
	now := time.Now()
	for _, id := range sessions {
		if !s.workloadSessionAllowed(ctx, id) {
			s.revokeSession(ctx, id, now)
			s.workloadLog().Info("pds_workload_session_revoked", "account", account, "role", role)
		}
	}
}

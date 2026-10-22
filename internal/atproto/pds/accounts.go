package pds

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
)

type createAccountRequest struct {
	Handle      string         `json:"handle"`
	Email       string         `json:"email,omitempty"`
	Password    string         `json:"password,omitempty"`
	InviteCode  string         `json:"inviteCode,omitempty"`
	DID         string         `json:"did,omitempty"`
	RecoveryKey string         `json:"recoveryKey,omitempty"`
	PLCOp       map[string]any `json:"plcOp,omitempty"`
}

type createSessionRequest struct {
	Identifier string `json:"identifier,omitempty"`
	Password   string `json:"password,omitempty"`
}

type sessionOutput struct {
	AccessJWT  string `json:"accessJwt"`
	RefreshJWT string `json:"refreshJwt"`
	Handle     string `json:"handle"`
	DID        string `json:"did"`
	Email      string `json:"email,omitempty"`
	Active     bool   `json:"active"`
	DIDDoc     any    `json:"didDoc,omitempty"`
}

type getSessionOutput struct {
	DID            string `json:"did"`
	Handle         string `json:"handle"`
	Email          string `json:"email,omitempty"`
	EmailConfirmed bool   `json:"emailConfirmed,omitempty"`
	Active         bool   `json:"active"`
	DIDDoc         any    `json:"didDoc,omitempty"`
}

type describeServerOutput struct {
	DID                       string         `json:"did"`
	AvailableUserDomains      []string       `json:"availableUserDomains"`
	InviteCodeRequired        bool           `json:"inviteCodeRequired"`
	PhoneVerificationRequired bool           `json:"phoneVerificationRequired"`
	Contact                   *serverContact `json:"contact,omitempty"`
}

type serverContact struct {
	Email string `json:"email,omitempty"`
}

type createAppPasswordRequest struct {
	Name       string `json:"name"`
	Privileged bool   `json:"privileged,omitempty"`
}

type revokeAppPasswordRequest struct {
	Name string `json:"name"`
}

type appPasswordOutput struct {
	Name       string    `json:"name"`
	Password   string    `json:"password,omitempty"`
	Privileged bool      `json:"privileged,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type appPasswordView struct {
	Name       string    `json:"name"`
	Privileged bool      `json:"privileged,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type listAppPasswordsOutput struct {
	Passwords []appPasswordView `json:"passwords"`
}

func (s *Server) handleDescribeServer(w http.ResponseWriter, _ *http.Request) {
	domains := append([]string(nil), s.cfg.AvailableUserDomains...)
	if len(domains) == 0 {
		domain := "localhost"
		if s.cfg.PublicBaseURL != "" {
			if u, err := url.Parse(s.cfg.PublicBaseURL); err == nil && u.Hostname() != "" {
				domain = u.Hostname()
			}
		}
		domains = []string{domain}
	}
	out := describeServerOutput{
		DID:                  s.serviceDID,
		AvailableUserDomains: domains,
	}
	if s.cfg.ContactEmail != "" {
		out.Contact = &serverContact{Email: s.cfg.ContactEmail}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.mode != ModeFull || s.store == nil {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "account creation is disabled")
		return
	}
	var req createAccountRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	handle, err := syntax.ParseHandle(strings.ToLower(strings.TrimSpace(req.Handle)))
	if err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidHandle", "invalid handle")
		return
	}
	if req.Password == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidPassword", "password is required")
		return
	}
	email := strings.TrimSpace(req.Email)
	if email != "" {
		addr, err := mail.ParseAddress(email)
		if err != nil || !strings.EqualFold(addr.Address, email) {
			writeXRPCError(w, http.StatusBadRequest, "InvalidEmail", "invalid email")
			return
		}
	}
	if s.keys == nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "repo key store is not configured")
		return
	}
	key, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "failed to generate signing key")
		return
	}
	pub, err := key.PublicKey()
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalServerError", "failed to derive signing key")
		return
	}
	signingDIDKey := pub.DIDKey()
	var did syntax.DID
	if req.DID != "" {
		if s.plc != nil && !s.cfg.AllowExplicitDID {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "DID must be created through the PLC directory")
			return
		}
		if !s.cfg.AllowExplicitDID {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "explicit DID creation is not allowed")
			return
		}
		did, err = syntax.ParseDID(req.DID)
		if err != nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid DID")
			return
		}
	} else {
		if s.plc == nil {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "PLC directory is not configured")
			return
		}
		result, err := s.plc.SubmitGenesis(ctx, PLCGenesisRequest{
			Handle:              handle.String(),
			Service:             s.publicURL(r),
			SigningKey:          signingDIDKey,
			RotationKey:         s.cfg.RotationKey,
			VerificationMethods: map[string]string{"atproto": signingDIDKey},
			AlsoKnownAs:         []string{"at://" + handle.String()},
		})
		if err != nil {
			writeXRPCError(w, http.StatusInternalServerError, "InternalError", "PLC genesis failed: "+err.Error())
			return
		}
		did, err = syntax.ParseDID(result.DID.String())
		if err != nil {
			writeXRPCError(w, http.StatusInternalServerError, "InternalError", "PLC returned an invalid DID")
			return
		}
	}
	acct, err := s.store.CreateAccount(ctx, NewAccount{
		DID:       did,
		Handle:    handle.String(),
		Email:     email,
		Password:  req.Password,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrHandleTaken):
			writeXRPCError(w, http.StatusBadRequest, "HandleNotAvailable", "handle is not available")
		case errors.Is(err, ErrAccountExists):
			writeXRPCError(w, http.StatusBadRequest, "DidExists", "DID is already registered")
		default:
			writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to create account")
		}
		return
	}
	if err := s.keys.PutRepoKey(ctx, did, key); err != nil {
		_ = s.store.DeleteAccount(ctx, did)
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to store repo signing key")
		return
	}
	if _, err := s.repos.CreateRepo(ctx, did, key); err != nil {
		_ = s.keys.DeleteRepoKey(ctx, did)
		_ = s.store.DeleteAccount(ctx, did)
		if errors.Is(err, repo.ErrRepoExists) {
			writeXRPCError(w, http.StatusBadRequest, "DidExists", "repo already exists")
			return
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to create repo")
		return
	}
	pair, err := s.createSession(ctx, acct, false, false)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, sessionOutput{
		AccessJWT:  pair.Access,
		RefreshJWT: pair.Refresh,
		Handle:     acct.Handle,
		DID:        acct.DID.String(),
		Email:      acct.Email,
		Active:     true,
		DIDDoc:     s.didDocument(ctx, acct.DID),
	})
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createSessionRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	if req.Identifier == "" || req.Password == "" {
		if user, pass, ok := r.BasicAuth(); ok {
			req.Identifier = user
			req.Password = pass
		}
	}
	if req.Identifier == "" || req.Password == "" {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "identifier and password are required")
		return
	}
	acct, appPassword, privileged, err := s.store.VerifyPassword(ctx, req.Identifier, req.Password)
	if err != nil {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid identifier or password")
		return
	}
	pair, err := s.createSession(ctx, acct, appPassword, privileged)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to create session")
		return
	}
	writeJSON(w, http.StatusOK, sessionOutput{
		AccessJWT:  pair.Access,
		RefreshJWT: pair.Refresh,
		Handle:     acct.Handle,
		DID:        acct.DID.String(),
		Email:      acct.Email,
		Active:     true,
		DIDDoc:     s.didDocument(ctx, acct.DID),
	})
}

func (s *Server) handleRefreshSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scheme, token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "refresh token required")
		return
	}
	claims, err := s.parseToken(token, scopeRefresh)
	if err != nil {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid refresh token")
		return
	}
	sess, err := s.store.GetLegacySessionByRefresh(ctx, claims.ID)
	if err != nil || sess == nil || sess.RevokedAt != nil || !sess.ExpiresAt.After(time.Now()) {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid refresh token")
		return
	}
	if sess.ID != claims.SID || sess.DID.String() != claims.Issuer {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid refresh token")
		return
	}
	acct, err := s.store.GetAccount(ctx, sess.DID)
	if err != nil || acct == nil || !acct.Active {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "account is not active")
		return
	}
	pair, err := s.issueTokenPair(sess.DID, sess.ID)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to issue token")
		return
	}
	if err := s.store.RotateLegacySession(ctx, sess.ID, pair.RefreshJTI, pair.RefreshExpires); err != nil {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid refresh token")
		return
	}
	writeJSON(w, http.StatusOK, sessionOutput{
		AccessJWT:  pair.Access,
		RefreshJWT: pair.Refresh,
		Handle:     acct.Handle,
		DID:        acct.DID.String(),
		Email:      acct.Email,
		Active:     true,
		DIDDoc:     s.didDocument(ctx, acct.DID),
	})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if sess, ok := oauth.SessionFromContext(r.Context()); ok && sess != nil {
		if err := s.store.RevokeSession(ctx, sess.ID, time.Now()); err != nil {
			writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to revoke session")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	scheme, token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "refresh token required")
		return
	}
	claims, err := s.parseToken(token, scopeRefresh)
	if err != nil {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "invalid refresh token")
		return
	}
	if err := s.store.RevokeLegacySessionByRefresh(ctx, claims.ID, time.Now()); err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to revoke session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	acct, err := s.store.GetAccount(ctx, info.DID)
	if err != nil || acct == nil {
		writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "account not found")
		return
	}
	writeJSON(w, http.StatusOK, getSessionOutput{
		DID:            acct.DID.String(),
		Handle:         acct.Handle,
		Email:          acct.Email,
		EmailConfirmed: acct.Email != "",
		Active:         acct.Active,
		DIDDoc:         s.didDocument(ctx, acct.DID),
	})
}

func (s *Server) handleCreateAppPassword(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	if info.AppPassword {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "app passwords cannot manage app passwords")
		return
	}
	var req createAppPasswordRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	password, err := GenerateAppPassword()
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to generate app password")
		return
	}
	app, err := s.store.CreateAppPassword(ctx, info.DID, req.Name, password, req.Privileged)
	if err != nil {
		if errors.Is(err, ErrAppPasswordExists) {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "app password name already exists")
			return
		}
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, appPasswordOutput{
		Name:       app.Name,
		Password:   password,
		Privileged: app.Privileged,
		CreatedAt:  app.CreatedAt,
	})
}

func (s *Server) handleListAppPasswords(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	if info.AppPassword {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "app passwords cannot list app passwords")
		return
	}
	list, err := s.store.ListAppPasswords(ctx, info.DID)
	if err != nil {
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to list app passwords")
		return
	}
	out := listAppPasswordsOutput{Passwords: make([]appPasswordView, 0, len(list))}
	for _, app := range list {
		out.Passwords = append(out.Passwords, appPasswordView{
			Name:       app.Name,
			Privileged: app.Privileged,
			CreatedAt:  app.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeAppPassword(w http.ResponseWriter, r *http.Request, info *authInfo) {
	ctx := r.Context()
	if info.AppPassword {
		writeXRPCError(w, http.StatusForbidden, "Forbidden", "app passwords cannot revoke app passwords")
		return
	}
	var req revokeAppPasswordRequest
	if err := s.decodeJSON(w, r, &req); err != nil {
		writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "invalid JSON")
		return
	}
	if err := s.store.RevokeAppPassword(ctx, info.DID, req.Name, time.Now()); err != nil {
		if errors.Is(err, ErrAppPasswordMissing) {
			writeXRPCError(w, http.StatusBadRequest, "InvalidRequest", "app password not found")
			return
		}
		writeXRPCError(w, http.StatusInternalServerError, "InternalError", "failed to revoke app password")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

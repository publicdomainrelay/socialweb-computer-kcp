package pds

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/repo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/serviceauth"
)

const (
	scopeAccess  = "com.atproto.access"
	scopeRefresh = "com.atproto.refresh"
)

type sessionClaims struct {
	Scope string `json:"scope"`
	SID   string `json:"sid"`
	jwt.RegisteredClaims
}

type tokenPair struct {
	Access         string
	Refresh        string
	RefreshJTI     string
	AccessExpires  time.Time
	RefreshExpires time.Time
}

type authInfo struct {
	DID         syntax.DID
	SessionID   string
	AppPassword bool
	Privileged  bool
	OAuth       bool
	Service     bool
}

func (s *Server) mintToken(did syntax.DID, sid, jti, scope string, ttl time.Duration) (string, error) {
	now := time.Now().UTC()
	claims := sessionClaims{
		Scope: scope,
		SID:   sid,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    did.String(),
			Subject:   did.String(),
			Audience:  jwt.ClaimStrings{s.audience},
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        jti,
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.sessionSecret)
	if err != nil {
		return "", fmt.Errorf("pds: signing token: %w", err)
	}
	return token, nil
}

func (s *Server) parseToken(token, scope string) (*sessionClaims, error) {
	claims := &sessionClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.sessionSecret, nil
	}, jwt.WithAudience(s.audience), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || !parsed.Valid {
		return nil, ErrAuthentication
	}
	if claims.Scope != scope || claims.SID == "" || claims.ID == "" {
		return nil, ErrAuthentication
	}
	if claims.Issuer == "" || claims.Subject != claims.Issuer {
		return nil, ErrAuthentication
	}
	if _, err := syntax.ParseDID(claims.Issuer); err != nil {
		return nil, ErrAuthentication
	}
	return claims, nil
}

func (s *Server) issueTokenPair(did syntax.DID, sid string) (*tokenPair, error) {
	refreshJTI, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	accessJTI, err := randomToken(16)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	refreshTTL := s.refreshTTL
	access, err := s.mintToken(did, sid, accessJTI, scopeAccess, s.accessTTL)
	if err != nil {
		return nil, err
	}
	refresh, err := s.mintToken(did, sid, refreshJTI, scopeRefresh, refreshTTL)
	if err != nil {
		return nil, err
	}
	return &tokenPair{
		Access:         access,
		Refresh:        refresh,
		RefreshJTI:     refreshJTI,
		AccessExpires:  now.Add(s.accessTTL),
		RefreshExpires: now.Add(refreshTTL),
	}, nil
}

func (s *Server) createSession(ctx context.Context, acct *Account, appPassword, privileged bool) (*tokenPair, error) {
	sid, err := randomToken(24)
	if err != nil {
		return nil, err
	}
	pair, err := s.issueTokenPair(acct.DID, sid)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	sess := LegacySession{
		ID:          sid,
		DID:         acct.DID,
		Handle:      acct.Handle,
		RefreshJTI:  pair.RefreshJTI,
		AppPassword: appPassword,
		Privileged:  privileged,
		CreatedAt:   now,
		ExpiresAt:   pair.RefreshExpires,
	}
	if err := s.store.CreateLegacySession(ctx, sess); err != nil {
		return nil, err
	}
	return pair, nil
}

func (s *Server) authenticate(r *http.Request) (*authInfo, error) {
	if s.store == nil {
		return nil, ErrAuthentication
	}
	if sess, ok := oauth.SessionFromContext(r.Context()); ok && sess != nil && sess.RevokedAt == nil && sess.ExpiresAt.After(time.Now()) {
		return &authInfo{
			DID:       sess.Account.DID,
			SessionID: sess.ID,
			OAuth:     true,
		}, nil
	}
	scheme, token, ok := serviceauth.SplitAuthorization(r.Header.Get("Authorization"))
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, ErrAuthentication
	}
	if claims, err := s.parseToken(token, scopeAccess); err == nil {
		sess, err := s.store.GetLegacySession(r.Context(), claims.SID)
		if err != nil || sess == nil || sess.RevokedAt != nil || !sess.ExpiresAt.After(time.Now()) || sess.DID.String() != claims.Issuer {
			return nil, ErrAuthentication
		}
		acct, err := s.store.GetAccount(r.Context(), sess.DID)
		if err != nil || acct == nil || !acct.Active {
			return nil, ErrAuthentication
		}
		return &authInfo{
			DID:         sess.DID,
			SessionID:   sess.ID,
			AppPassword: sess.AppPassword,
			Privileged:  sess.Privileged,
		}, nil
	}
	if s.verifier != nil {
		did, err := s.verifier.VerifyRequest(r)
		if err == nil {
			return &authInfo{DID: did, Service: true}, nil
		}
	}
	return nil, ErrAuthentication
}

func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request, *authInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, err := s.authenticate(r)
		if err != nil {
			writeXRPCError(w, http.StatusUnauthorized, "AuthenticationRequired", "authentication required")
			return
		}
		next(w, r, info)
	}
}

func (s *Server) resolveRepo(ctx context.Context, raw string) (syntax.DID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrAccountNotFound
	}
	if strings.HasPrefix(raw, "did:") {
		did, err := syntax.ParseDID(raw)
		if err != nil {
			return "", fmt.Errorf("%w: %v", repo.ErrInvalidRequest, err)
		}
		return did, nil
	}
	handle, err := syntax.ParseHandle(strings.ToLower(raw))
	if err != nil {
		return "", fmt.Errorf("%w: %v", repo.ErrInvalidRequest, err)
	}
	if s.store != nil {
		acct, err := s.store.GetAccountByIdentifier(ctx, handle.String())
		if err == nil && acct != nil {
			return acct.DID, nil
		}
	}
	if s.resolver != nil {
		did, err := s.resolver.ResolveHandle(ctx, handle)
		if err == nil {
			return did, nil
		}
	}
	return "", repo.ErrRepoNotFound
}

func (s *Server) accountHandle(ctx context.Context, did syntax.DID) string {
	if s.store != nil {
		acct, err := s.store.GetAccount(ctx, did)
		if err == nil && acct != nil {
			return acct.Handle
		}
	}
	if s.resolver != nil {
		if doc, err := s.resolver.ResolveDID(ctx, did); err == nil && doc != nil {
			for _, aka := range doc.AlsoKnownAs {
				if after, ok := strings.CutPrefix(aka, "at://"); ok {
					handle := after
					if _, err := syntax.ParseHandle(handle); err == nil {
						return handle
					}
				}
			}
		}
	}
	return did.String()
}

func (s *Server) handleIsCorrect(ctx context.Context, did syntax.DID, handle string) bool {
	if handle == "" || handle == did.String() {
		return false
	}
	if s.store != nil {
		acct, err := s.store.GetAccount(ctx, did)
		if err == nil && acct != nil {
			if strings.EqualFold(acct.Handle, handle) {
				return true
			}
		}
	}
	if s.resolver != nil {
		resolved, err := s.resolver.ResolveHandle(ctx, syntax.Handle(handle))
		if err == nil && resolved == did {
			return true
		}
	}
	return false
}

func (s *Server) didDocument(ctx context.Context, did syntax.DID) any {
	if s.resolver == nil {
		return nil
	}
	doc, err := s.resolver.ResolveDID(ctx, did)
	if err != nil {
		return nil
	}
	return doc
}

func (s *Server) publicURL(r *http.Request) string {
	if s.cfg.PublicBaseURL != "" {
		return strings.TrimRight(s.cfg.PublicBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto != "" {
		scheme = strings.ToLower(proto)
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	if scheme == "http" && !isLoopbackHost(host) && r.Header.Get("X-Forwarded-Proto") == "" {
		scheme = "https"
	}
	return scheme + "://" + host
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

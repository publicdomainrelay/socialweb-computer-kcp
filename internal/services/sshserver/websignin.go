package sshserver

import (
	"bytes"
	"context"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/ownersession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const vaultObjectKind = "Vault"

var oauthRoutes = []string{"GET /oauth-client-metadata.json", "POST /oauth/start", "GET " + gatewayhttp.WebOAuthCallbackPath, "GET /session", "DELETE /session"}

type liveAuth struct {
	s *Service
}

type liveAgents struct {
	s *Service
}

var (
	_ sshtransport.Authenticator = liveAuth{}
	_ sshtransport.AgentProvider = liveAgents{}
)

func (a liveAuth) Authenticate(r *http.Request) (*gatewayhttp.WebCaller, error) {
	web := a.s.currentWeb()
	if web == nil {
		return nil, nil
	}
	return web.auth.Authenticate(r)
}

func (a liveAgents) AgentFor(caller *gatewayhttp.WebCaller) gatewayhttp.WebAccountAgent {
	web := a.s.currentWeb()
	if web == nil {
		return ownersession.Unavailable{}
	}
	return web.signIn.AgentFor(caller)
}

func (s *Service) currentWeb() *webSignIn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.web
}

func (s *Service) oauthRoute(w http.ResponseWriter, r *http.Request) {
	web := s.currentWeb()
	if web == nil {
		http.Error(w, "the sign-in is not available", http.StatusServiceUnavailable)
		return
	}
	web.handler.ServeHTTP(w, r)
}

func sameEndpoint(a, b *webSignIn) bool {
	return a != nil && b != nil && a.endpoint.Address == b.endpoint.Address && bytes.Equal(a.endpoint.CACert, b.endpoint.CACert) &&
		bytes.Equal(a.endpoint.ClientCert, b.endpoint.ClientCert) && bytes.Equal(a.endpoint.ClientKey, b.endpoint.ClientKey)
}

func (s *Service) followVault(ctx context.Context) func() {
	if s.spec.VaultRef == nil || s.deps.Bus == nil || s.currentWeb() == nil {
		return func() {}
	}
	workspace, name := s.workspace(), s.spec.VaultRef.Name
	wake := make(chan struct{}, 1)
	cancel := s.deps.Bus.Subscribe(func(key bus.Key) {
		if key.Kind != vaultObjectKind || key.Name != name || key.Workspace != workspace {
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				s.refreshSignIn(ctx)
			}
		}
	}()
	return cancel
}

func (s *Service) refreshSignIn(ctx context.Context) {
	next, err := s.openSignIn(ctx)
	if err != nil || next == nil {
		if err != nil {
			s.log.Warn("sshserver_sign_in_refresh_failed", "err", err.Error())
		}
		return
	}
	s.mu.Lock()
	old := s.web
	if sameEndpoint(old, next) {
		s.mu.Unlock()
		next.close()
		return
	}
	s.web = next
	s.mu.Unlock()
	old.close()
	s.log.Info("sshserver_sign_in_follows_vault", "address", next.endpoint.Address)
}

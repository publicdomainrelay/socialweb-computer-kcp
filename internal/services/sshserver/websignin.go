package sshserver

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/custody"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ssh/ownersession"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sshtransport"
)

const (
	vaultObjectKind = "Vault"

	signInRetryAttempts = 6

	signInRetryMax = 30 * time.Second
)

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

func (s *Service) awaitVault(ctx context.Context) error {
	if s.spec.VaultRef == nil || s.spec.VaultRef.Name == "" || s.deps.Bus == nil {
		return nil
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
	defer cancel()
	waiting := false
	for {
		_, err := s.vaultEndpoint(ctx)
		switch {
		case err == nil:
			if waiting {
				s.log.Info("sshserver_vault_ready", "vault", workspace+"/"+name)
			}
			return nil
		case !errors.Is(err, custody.ErrNotReady):
			return err
		case !waiting:
			waiting = true
			s.log.Info("sshserver_waits_for_vault", "vault", workspace+"/"+name, "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		}
	}
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
			}
			for attempt := 1; !s.refreshSignIn(ctx) && attempt < signInRetryAttempts; attempt++ {
				select {
				case <-ctx.Done():
					return
				case <-wake:
				case <-time.After(signInRetryDelay(attempt)):
				}
			}
		}
	}()
	return cancel
}

func signInRetryDelay(attempt int) time.Duration {
	return min(time.Second<<(attempt-1), signInRetryMax)
}

func (s *Service) refreshSignIn(ctx context.Context) bool {
	next, err := s.openSignIn(ctx)
	if err != nil {
		s.log.Warn("sshserver_sign_in_refresh_failed", "err", err.Error())
		return false
	}
	if next == nil {
		return true
	}
	s.mu.Lock()
	old := s.web
	if sameEndpoint(old, next) {
		s.mu.Unlock()
		next.close()
		return true
	}
	s.web = next
	s.mu.Unlock()
	old.close()
	s.log.Info("sshserver_sign_in_follows_vault", "address", next.endpoint.Address)
	return true
}

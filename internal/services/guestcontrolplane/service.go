package guestcontrolplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

type Service struct {
	cfg Config

	log *slog.Logger

	mu sync.Mutex

	started bool

	registered bool

	mux *http.ServeMux

	cancel context.CancelFunc

	wg sync.WaitGroup

	stopOnce sync.Once

	stopErr error

	cleanupMu sync.Mutex

	endpoints []component.Endpoint

	store *kcpstore.Store

	cluster string

	did string

	dir identity.Directory

	identity *guestidentity.Client

	verifier *guestidentity.Verifier

	guest *guestidentity.Guest

	vms VMStore

	grants GuestStore

	rbac guestidentity.RBACSource

	syncer *guestidentity.Syncer

	grantsLoop *grantLoop

	relay *ingress.Subscriber

	issuer atomic.Value
}

func (s *Service) Start(ctx context.Context, deps component.Deps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.cfg.Deps = deps
	if deps.Log != nil {
		s.log = deps.Log.With("component", "guestcontrolplane", "handle", s.cfg.Handle)
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		cancel()
		return errors.New("guestcontrolplane: the service is already started")
	}
	s.started = true
	s.cancel = cancel
	s.mu.Unlock()
	defer s.shutdown()

	if err := s.prepare(runCtx); err != nil {
		cancel()
		return err
	}
	<-runCtx.Done()
	return nil
}

func (s *Service) Ready(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	registered := s.registered
	s.mu.Unlock()
	if !started || !registered {
		return errors.New("guestcontrolplane: the service is not registered")
	}
	if s.guest == nil {
		return errors.New("guestcontrolplane: the guest identity proxy is not configured")
	}
	return nil
}

func (s *Service) Endpoints() []component.Endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]component.Endpoint(nil), s.endpoints...)
}

func (s *Service) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.shutdownWith(ctx)
}

func (s *Service) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), component.DefaultStopTimeout)
	defer cancel()
	return s.shutdownWith(ctx)
}

func (s *Service) shutdownWith(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.cleanupMu.Lock()
		defer s.cleanupMu.Unlock()
		s.mu.Lock()
		cancel := s.cancel
		relay := s.relay
		s.started = false
		s.registered = false
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if relay != nil {
			relay.Close()
		}
		if s.guest != nil {
			s.guest.Close()
		}
		if s.syncer != nil {
			s.syncer.Close()
		}
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-ctx.Done():
			if s.stopErr == nil {
				s.stopErr = ctx.Err()
			}
		}
	})
	return s.stopErr
}

func (s *Service) prepare(ctx context.Context) error {
	if s.cfg.StateDir != "" {
		if err := os.MkdirAll(s.cfg.StateDir, 0o700); err != nil {
			return fmt.Errorf("guestcontrolplane: creating the state dir: %w", err)
		}
	}
	if s.cfg.VMs == nil || s.cfg.Grants == nil || s.cfg.OpenBao == nil {
		if err := s.resolveKCP(ctx); err != nil {
			s.log.Warn("the guest control plane runs without some kcp stores", "err", err)
		}
	}
	s.dir = ingress.NewDirectory(s.cfg.Network.PlcDirectoryUrl)
	if err := s.resolveIdentity(ctx); err != nil {
		s.log.Warn("the bidder identity is not resolved yet", "err", err)
	}
	s.resolveStores()
	endpoint := s.endpointResolver()
	identityClient, err := guestidentity.NewClient(guestidentity.Options{
		Mount:    s.cfg.GuestLoginMount,
		Endpoint: endpoint,
		Timeout:  DefaultOpenBaoTimeout,
	})
	if err != nil {
		return err
	}
	s.identity = identityClient
	s.verifier = guestidentity.NewVerifier(guestidentity.IdentityKeys(identityClient), func() string {
		return guestidentity.OIDCIssuer(s.issuerURL())
	})
	if s.cfg.RBAC == nil {
		s.cfg.RBAC = &guestidentity.HTTPRBAC{
			Resolver: guestidentity.DirectoryPDS{Directory: s.dir},
			Client:   networkHTTPClient(s.cfg.Deps, s.cfg.Network, DefaultOpenBaoTimeout),
			Log:      s.log,
		}
	}
	s.rbac = s.cfg.RBAC
	if s.cfg.Grants == nil {
		s.log.Warn("the guest identity grants cannot sync without a grant store")
	} else {
		syncer, err := guestidentity.NewSyncer(ctx, guestidentity.SyncerOptions{
			Store:  s.cfg.Grants,
			RBAC:   s.rbac,
			Issuer: func() string { return guestidentity.OIDCIssuer(s.issuerURL()) },
			Log:    s.log,
			Resync: s.cfg.GrantSync,
		})
		if err != nil {
			return err
		}
		s.syncer = syncer
	}
	guest, err := guestidentity.NewGuest(ctx, guestidentity.GuestOptions{
		Handle:          s.cfg.Handle,
		Identity:        identityClient,
		Verifier:        s.verifier,
		VMs:             s.vms,
		Log:             s.log,
		Now:             s.cfg.Now,
		HostKeyAttempts: s.cfg.HostKeyAttempts,
		HostKeyRetry:    s.cfg.HostKeyRetry,
	})
	if err != nil {
		return err
	}
	s.guest = guest
	s.mux = s.routes()
	if err := s.register(); err != nil {
		return err
	}
	s.addEndpoint(component.Endpoint{Name: "http", Protocol: component.ProtocolHTTP})
	if err := s.startRelay(ctx); err != nil {
		s.log.Warn("the guest relay did not start", "err", err)
	}
	s.setIssuer(ctx)
	if raw := s.issuerURL(); raw != "" {
		s.addEndpoint(component.Endpoint{
			Name:     "issuer",
			Protocol: component.ProtocolHTTPS,
			URL:      guestidentity.OIDCIssuer(raw),
		})
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.configureIssuerLoop(ctx)
	}()
	if s.syncer != nil {
		s.grantsLoop = &grantLoop{syncer: s.syncer, vms: s.vms, guest: s.guest, did: s.did, handle: "",
			interval: s.cfg.GrantSync, log: s.log, known: map[string]string{}}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.grantsLoop.run(ctx)
		}()
	}
	s.log.Info("the guest control plane serves guests", "handler", s.logicalURL(), "handle", s.cfg.Handle,
		"issuer", s.issuerURL(), "mount", s.cfg.GuestLoginMount)
	return nil
}

func (s *Service) register() error {
	if s.cfg.Deps.Handlers == nil {
		return errors.New("guestcontrolplane: the handler registry is not available")
	}
	key := component.Key{Workspace: s.cfg.Workspace, Kind: Kind, Name: s.cfg.ComponentName}
	if err := s.cfg.Deps.Handlers.RegisterHTTP(key, s.mux); err != nil {
		return err
	}
	s.mu.Lock()
	s.registered = true
	s.mu.Unlock()
	return nil
}

func (s *Service) logicalURL() string {
	return handlerURL(s.cfg.Workspace, Kind, s.cfg.ComponentName)
}

func (s *Service) resolveKCP(ctx context.Context) error {
	if s.cfg.Deps.KCP == nil {
		return nil
	}
	store, cluster, err := kcpStore(s.cfg.Deps, s.cfg.Workspace)
	if err != nil {
		return err
	}
	s.store = store
	s.cluster = cluster
	return nil
}

func (s *Service) resolveIdentity(ctx context.Context) error {
	s.did = s.cfg.DID
	if s.store == nil || s.cfg.IdentityRef == nil || s.cfg.IdentityRef.Name == "" {
		return nil
	}
	identities := newClusterResource[corev1alpha1.MarketIdentity](s.store, MarketIdentityGVR)
	obj, err := identities.Get(ctx, s.cluster, s.cfg.IdentityRef.Name)
	if err != nil {
		return err
	}
	if s.did == "" {
		s.did = obj.Status.Did
	}
	if s.cfg.Handle == "" {
		s.cfg.Handle = obj.Status.Handle
	}
	return nil
}

func (s *Service) resolveStores() {
	if s.store != nil {
		if s.cfg.VMs == nil {
			s.cfg.VMs = NewKCPVMs(s.store, s.cluster, s.cfg.Handle)
		}
		if s.cfg.Grants == nil {
			s.cfg.Grants = NewKCPGrants(s.store, s.cluster)
		}
		if s.cfg.OpenBao == nil {
			s.cfg.OpenBao = &KCPOpenBao{Res: newClusterResource[corev1alpha1.OpenBaoServer](s.store, OpenBaoServerGVR), Cluster: s.cluster}
		}
	}
	s.vms = s.cfg.VMs
	s.grants = s.cfg.Grants
}

func (s *Service) endpointResolver() func(ctx context.Context) (guestidentity.Endpoint, error) {
	if s.cfg.Endpoint != nil {
		return s.cfg.Endpoint
	}
	source := s.cfg.OpenBao
	server := s.cfg.VaultServer
	client := s.cfg.VaultClient
	return func(ctx context.Context) (guestidentity.Endpoint, error) {
		if source == nil {
			return guestidentity.Endpoint{}, errors.New("guestcontrolplane: no OpenBaoServer source is configured")
		}
		status, err := source.Server(ctx, server)
		if err != nil {
			return guestidentity.Endpoint{}, err
		}
		return guestidentity.EndpointFromServer(status, client)
	}
}

func (s *Service) setIssuer(ctx context.Context) {
	if s.cfg.Issuer != "" {
		s.issuer.Store(s.cfg.Issuer)
		return
	}
	if raw := s.exposureIssuerURL(ctx); raw != "" {
		s.issuer.Store(raw)
		return
	}
	if raw := s.relayURL(); raw != "" {
		s.issuer.Store(strings.TrimRight(raw, "/"))
		return
	}
	s.issuer.Store("")
}

var exposureGVR = schema.GroupVersionResource{Group: "exposure.socialweb.computer", Version: "v1alpha1", Resource: "exposures"}

func (s *Service) exposureClient() (dynamic.Interface, error) {
	if s.cfg.Deps.KCP == nil {
		return nil, errors.New("guestcontrolplane: the component has no kcp clients")
	}
	client, err := s.cfg.Deps.KCP.Client(s.cfg.Workspace)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(client.RESTConfig())
	baseHost, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = baseHost + "/clusters/" + s.cfg.Workspace
	return dynamic.NewForConfig(cfg)
}

func (s *Service) exposureIssuerURL(ctx context.Context) string {
	name := "svc-guestcontrolplane-" + strings.TrimSpace(s.cfg.ComponentName) + "-ingress"
	client, err := s.exposureClient()
	if err != nil {
		return ""
	}
	u, err := client.Resource(exposureGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "url")
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return raw
}

func (s *Service) issuerURL() string {
	value, _ := s.issuer.Load().(string)
	return strings.TrimRight(value, "/")
}

func (s *Service) addEndpoint(endpoint component.Endpoint) {
	s.mu.Lock()
	s.endpoints = append(s.endpoints, endpoint)
	s.mu.Unlock()
}

func (s *Service) configureIssuerLoop(ctx context.Context) {
	if s.identity == nil {
		return
	}
	delay := s.cfg.IssuerRetry
	if delay <= 0 {
		delay = DefaultIssuerRetry
	}
	for {
		issuer := s.issuerURL()
		if issuer == "" {
			if raw := s.exposureIssuerURL(ctx); raw != "" {
				s.issuer.Store(raw)
				issuer = raw
			}
		}
		if issuer == "" {
			s.log.Info("the guest OIDC issuer waits for the guest control plane ingress exposure URL")
		} else {
			attempt, cancel := context.WithTimeout(ctx, DefaultOpenBaoTimeout)
			err := s.identity.ConfigureIssuer(attempt, issuer)
			cancel()
			if err == nil {
				s.log.Info("the guest OIDC issuer is configured", "issuer", guestidentity.OIDCIssuer(issuer), "issuerUrl", guestidentity.OIDCIssuer(issuer))
				return
			}
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("configuring the guest OIDC issuer failed; retrying", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

var _ component.Service = (*Service)(nil)

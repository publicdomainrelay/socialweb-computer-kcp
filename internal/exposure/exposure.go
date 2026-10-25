package exposure

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/servetls"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/tlsreload"
)

type PublisherConfig struct {
	Handlers  *component.Handlers
	Ports     *component.PortRegistry
	TLS       *tls.Config
	Registrar IngressRegistrar
	Log       *slog.Logger
}

type Binding struct {
	Workspace string

	Name string

	Spec exposurev1alpha1.ExposureSpec

	Service exposurev1alpha1.ServiceSpec

	ServiceGeneration int64

	Target component.Key

	SubscriberChanged func()
}

type Published struct {
	Address string

	Endpoint string

	Provider string

	Ingress *exposurev1alpha1.ExposureIngress
}

const addrInUseRelease = 12

type Publisher struct {
	handlers  *component.Handlers
	ports     *component.PortRegistry
	tls       *tls.Config
	registrar IngressRegistrar
	log       *slog.Logger

	mu          sync.Mutex
	running     map[string]*publication
	busy        map[string]int
	subscribers map[string]exposurev1alpha1.ExposureIngressSubscriber
}

type publication struct {
	fingerprint string

	published Published

	caPool   *tlsreload.Pool
	caBundle string

	once sync.Once

	close func()
}

func (p *publication) shutdown() {
	if p == nil {
		return
	}
	p.once.Do(func() {
		if p.close != nil {
			p.close()
		}
	})
}

func NewPublisher(cfg PublisherConfig) *Publisher {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Publisher{handlers: cfg.Handlers, ports: cfg.Ports, tls: cfg.TLS, registrar: cfg.Registrar,
		log: log.With("component", "exposure-publisher"), running: map[string]*publication{}, busy: map[string]int{},
		subscribers: map[string]exposurev1alpha1.ExposureIngressSubscriber{}}
}

func (p *Publisher) Handler(ctx context.Context, target component.Key, protocol exposurev1alpha1.ServiceProtocol, name string) (http.Handler, bool, error) {
	return FindHandler(ctx, p.handlers, target, protocol, name)
}

func (p *Publisher) Publish(ctx context.Context, b Binding) (Published, error) {
	key := publicationKey(b.Workspace, b.Name)
	fingerprint, err := p.fingerprint(b)
	if err != nil {
		return Published{}, err
	}
	if current := p.current(key); current != nil && current.fingerprint == fingerprint {
		err := p.rotateCA(b.Name, current, b.Spec.TLS.CABundle)
		if err == nil {
			return current.published, nil
		}
		p.log.Warn("rebuilding the Exposure after a bad CA bundle", "workspace", b.Workspace, "exposure", b.Name, "err", err)
	}
	p.stop(key)
	var pub *publication
	switch b.Spec.Mode {
	case exposurev1alpha1.ExposureModeLocal:
		pub, err = p.startLocal(ctx, b)
	case exposurev1alpha1.ExposureModeIngress:
		pub, err = p.startIngress(ctx, b)
	default:
		err = fmt.Errorf("unknown mode %q", b.Spec.Mode)
	}
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) && p.addrInUse(key) {
			p.log.Warn("the kept port of the Exposure stays taken; it takes a new port", "workspace", b.Workspace, "exposure", b.Name)
			p.release(b.Workspace, b.Name)
		}
		return Published{}, err
	}
	p.mu.Lock()
	delete(p.busy, key)
	p.mu.Unlock()
	pub.fingerprint = fingerprint
	pub.caBundle = strings.TrimSpace(b.Spec.TLS.CABundle)
	p.mu.Lock()
	p.running[key] = pub
	p.mu.Unlock()
	return pub.published, nil
}

func (p *Publisher) addrInUse(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.busy[key]++
	if p.busy[key] < addrInUseRelease {
		return false
	}
	delete(p.busy, key)
	return true
}

func (p *Publisher) Withdraw(workspace, name string) {
	key := publicationKey(workspace, name)
	p.stop(key)
	p.release(workspace, name)
	p.mu.Lock()
	delete(p.subscribers, key)
	p.mu.Unlock()
}

func (p *Publisher) Subscriber(workspace, name string) (exposurev1alpha1.ExposureIngressSubscriber, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.subscribers[publicationKey(workspace, name)]
	return state, ok
}

func (p *Publisher) subscriberReporter(key string, changed func()) func(ingress.SubscriberStatus) {
	return func(status ingress.SubscriberStatus) {
		p.mu.Lock()
		p.subscribers[key] = SubscriberStateOf(status)
		p.mu.Unlock()
		if changed != nil {
			changed()
		}
	}
}

func (p *Publisher) Pause(workspace, name string) {
	p.stop(publicationKey(workspace, name))
}

func (p *Publisher) stop(key string) {
	p.mu.Lock()
	pub := p.running[key]
	delete(p.running, key)
	p.mu.Unlock()
	pub.shutdown()
}

func (p *Publisher) Close() {
	p.mu.Lock()
	running := p.running
	p.running = map[string]*publication{}
	p.mu.Unlock()
	for _, pub := range running {
		pub.shutdown()
	}
}

func publicationKey(workspace, name string) string {
	return workspace + "/" + name
}

func (p *Publisher) fingerprint(b Binding) (string, error) {
	spec := b.Spec
	spec.TLS.CABundle = caBundleKey(spec.TLS.CABundle)
	value := map[string]any{
		"exposure": map[string]any{"spec": spec},
		"service":  map[string]any{"spec": b.Service, "generation": b.ServiceGeneration},
		"target":   b.Target,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	revision := uint64(0)
	if p.handlers != nil {
		revision = p.handlers.HandlerRevision(b.Target, b.Service.Handler)
	}
	return fmt.Sprintf("%s|handler-%d", raw, revision), nil
}

func caBundleKey(bundle string) string {
	bundle = strings.TrimSpace(bundle)
	if isPEMBundle(bundle) {
		return "pem"
	}
	return bundle
}

func isPEMBundle(bundle string) bool {
	return strings.Contains(bundle, "-----BEGIN")
}

func (p *Publisher) rotateCA(name string, pub *publication, bundle string) error {
	bundle = strings.TrimSpace(bundle)
	p.mu.Lock()
	defer p.mu.Unlock()
	if bundle == pub.caBundle {
		return nil
	}
	if pub.caPool != nil && isPEMBundle(bundle) {
		if err := pub.caPool.Set([]byte(bundle)); err != nil {
			return err
		}
		p.log.Info("rotated the Exposure client CA bundle", "exposure", name)
	}
	pub.caBundle = bundle
	return nil
}

func (p *Publisher) current(key string) *publication {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running[key]
}

func (p *Publisher) release(workspace, name string) {
	if p.ports == nil {
		return
	}
	_ = p.ports.Release(leaseName(workspace, name))
}

func leaseName(workspace, name string) string {
	return "Exposure/" + workspace + "/" + name
}

func (p *Publisher) startLocal(ctx context.Context, b Binding) (*publication, error) {
	if p.ports == nil {
		return nil, errors.New("the port registry is not configured")
	}
	listener, lease, err := p.ports.Listen(leaseName(b.Workspace, b.Name))
	if err != nil {
		return nil, err
	}
	tlsConfig, caPool, err := p.localTLS(b.Spec.TLS)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	protocol := protocolKind(b.Service.Protocol)
	path := normalizePath(b.Spec.Path)
	target := b.Target
	pub := &publication{caPool: caPool}
	switch protocol {
	case component.ProtocolHTTP, component.ProtocolWS:
		if _, ok := p.handlers.Handler(target, b.Service.Handler); !ok {
			_ = listener.Close()
			return nil, errors.New("the target HTTP handler is not registered")
		}
		name := HandlerName(b.Service.Handler)
		current := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h, ok := p.handlers.HandlerNamed(target, name)
			if !ok {
				http.Error(w, "the target handler is not registered", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r.WithContext(component.WithCaller(r.Context(), target)))
		})
		server := &http.Server{Handler: current, ReadHeaderTimeout: 10 * time.Second}
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = server.Serve(listener)
		}()
		pub.close = func() {
			_ = server.Close()
			<-done
		}
		scheme := httpScheme(protocol, tlsConfig != nil)
		pub.published = Published{Address: scheme + "://" + lease.Address() + path, Endpoint: lease.Address()}
		return pub, nil
	case component.ProtocolTCP, component.ProtocolSSH:
		runCtx, cancel := context.WithCancel(bus.WithCaller(context.WithoutCancel(ctx), ExposureCaller(b.Workspace, b.Name)))
		done := make(chan struct{})
		var conns sync.WaitGroup
		go func() {
			defer close(done)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				conns.Go(func() { bridge(runCtx, p.handlers, target, b.Service.Handler, conn) })
			}
		}()
		pub.close = func() {
			cancel()
			_ = listener.Close()
			<-done
			conns.Wait()
		}
		scheme := cmp.Or(protocol, component.ProtocolTCP)
		pub.published = Published{Address: scheme + "://" + lease.Address(), Endpoint: lease.Address()}
		return pub, nil
	default:
		_ = listener.Close()
		return nil, fmt.Errorf("unknown protocol %q", b.Service.Protocol)
	}
}

func (p *Publisher) startIngress(ctx context.Context, b Binding) (*publication, error) {
	if p.registrar == nil {
		return nil, errors.New("no ingress registrar is configured")
	}
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	registration, err := p.registrar.Register(life, IngressSpec{
		Workspace:      b.Workspace,
		Exposure:       b.Name,
		Service:        strs.LastSegment(b.Spec.ServiceRef),
		Protocol:       b.Service.Protocol,
		Handler:        b.Service.Handler,
		Target:         b.Target,
		IngressRef:     b.Spec.IngressRef,
		Hostname:       b.Spec.Hostname,
		IngressAddress: b.Spec.IngressAddress,
		IngressDial:    b.Spec.IngressDial,
		PublicHost:     b.Spec.PublicHost,
		Path:           normalizePath(b.Spec.Path),
		TLS:            b.Spec.TLS.Enabled,
		AuthRequired:   b.Spec.Auth.Required,
		AllowedDIDs:    append([]string(nil), b.Spec.Auth.AllowedDIDs...),
		OnStatus:       p.subscriberReporter(publicationKey(b.Workspace, b.Name), b.SubscriberChanged),
	})
	if !stop() || err != nil {
		cancel()
		if err == nil {
			_ = registration.Close()
			err = ctx.Err()
		}
		return nil, err
	}
	address := registration.URL()
	provider := ""
	if described, ok := registration.(IngressRegistrationProvider); ok {
		provider = strings.TrimSpace(described.Provider())
	}
	if path := normalizePath(b.Spec.Path); path == "" || path == "/" {
		address = originURL(address)
	}
	var route *exposurev1alpha1.ExposureIngress
	if routed, ok := registration.(IngressRegistrationRoute); ok {
		value := routed.Route()
		route = &value
	}
	return &publication{published: Published{Address: address, Endpoint: address, Provider: provider, Ingress: route},
		close: func() {
			_ = registration.Close()
			cancel()
		}}, nil
}

func originURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (p *Publisher) localTLS(spec exposurev1alpha1.ExposureTLS) (*tls.Config, *tlsreload.Pool, error) {
	if !spec.Enabled && strings.TrimSpace(spec.CertRef) == "" {
		return nil, nil, nil
	}
	var config *tls.Config
	switch {
	case strings.TrimSpace(spec.CertRef) != "":
		certFile := strings.TrimSpace(spec.CertRef)
		reloader, err := tlsreload.New(certFile, []string{servetls.KeyFileFor(certFile)}, p.log)
		if err != nil {
			return nil, nil, fmt.Errorf("loading the certificate %s: %w", certFile, err)
		}
		config = reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	case p.tls != nil:
		config = p.tls.Clone()
		config.MinVersion = cmp.Or(config.MinVersion, tls.VersionTLS12)
	default:
		return nil, nil, errors.New("TLS is enabled but no certificate is available")
	}
	bundle := strings.TrimSpace(spec.CABundle)
	if bundle == "" {
		return config, nil, nil
	}
	pool, err := tlsreload.PoolFromBundle(bundle, p.log)
	if err != nil {
		return nil, nil, err
	}
	return pool.ServerConfig(config, tls.VerifyClientCertIfGiven), pool, nil
}

func bridge(ctx context.Context, handlers *component.Handlers, target component.Key, name string, conn net.Conn) {
	defer conn.Close()
	if handlers == nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	stream, err := handlers.DialStream(ctx, target, name)
	if err != nil {
		return
	}
	defer stream.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, conn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, stream)
		done <- struct{}{}
	}()
	<-done
	_ = conn.Close()
	_ = stream.Close()
	<-done
}

func normalizePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return value
}

func httpScheme(protocol string, secure bool) string {
	switch protocol {
	case component.ProtocolWS:
		if secure {
			return "wss"
		}
		return "ws"
	default:
		if secure {
			return "https"
		}
		return "http"
	}
}

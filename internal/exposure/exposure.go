package exposure

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsreload"
)

const (
	DefaultShutdownTimeout = 5 * time.Second
)

type ExposureOptions struct {
	Workspace  string
	Dynamic    DynamicFactory
	Handlers   *component.Handlers
	Registry   *component.Registry
	Ports      *component.PortRegistry
	TLS        *tls.Config
	Registrar  IngressRegistrar
	Log        *slog.Logger
	RetryAfter time.Duration
}

type ExposureController struct {
	workspace string
	dynamic   DynamicFactory
	handlers  *component.Handlers
	registry  *component.Registry
	ports     *component.PortRegistry
	tls       *tls.Config
	registrar IngressRegistrar
	log       *slog.Logger
	retry     time.Duration

	mu      sync.Mutex
	running map[string]*exposureRuntime
}

type exposureRuntime struct {
	fingerprint string

	provider string

	once sync.Once

	close func()
}

func (r *exposureRuntime) shutdown() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.close != nil {
			r.close()
		}
	})
}

func NewExposureController(opts ExposureOptions) (*ExposureController, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("exposure: a workspace is required for the Exposure controller")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("exposure: a dynamic client factory is required for the Exposure controller")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	return &ExposureController{
		workspace: workspace,
		dynamic:   opts.Dynamic,
		handlers:  opts.Handlers,
		registry:  opts.Registry,
		ports:     opts.Ports,
		tls:       opts.TLS,
		registrar: opts.Registrar,
		log:       log.With("component", "exposure-controller"),
		retry:     opts.RetryAfter,
		running:   map[string]*exposureRuntime{},
	}, nil
}

func (c *ExposureController) Run(ctx context.Context) error {
	w := &watcher{
		workspace: c.workspace,
		dynamic:   c.dynamic,
		gvr:       ExposureGVR,
		log:       c.log,
		retry:     c.retry,
		reconcile: c.Reconcile,
		deleted:   c.forget,
	}
	return w.Run(ctx)
}

func (c *ExposureController) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	ri := dyn.Resource(ExposureGVR)
	name := u.GetName()
	if u.GetDeletionTimestamp() != nil {
		c.stop(name)
		c.release(name)
		return ensureMetadata(ctx, ri, name, ExposureFinalizer, false, nil)
	}
	var exp exposurev1alpha1.Exposure
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &exp); err != nil {
		return fmt.Errorf("exposure: decoding Exposure %s: %w", name, err)
	}
	labels := map[string]string{}
	if serviceName := normalizeName(exp.Spec.ServiceRef); serviceName != "" {
		labels[LabelService] = serviceName
	}
	if err := ensureMetadata(ctx, ri, name, ExposureFinalizer, true, labels); err != nil {
		return err
	}
	generation := u.GetGeneration()
	status := exp.Status
	status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	if exp.Spec.Suspend {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "Suspended", "the Exposure is suspended")
		setCondition(&status.Conditions, generation, ConditionReady, false, "Suspended", "the Exposure is suspended")
		return writeStatus(ctx, ri, name, &status)
	}
	if exp.Spec.Mode != exposurev1alpha1.ExposureModeLocal && exp.Spec.Mode != exposurev1alpha1.ExposureModeIngress {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposureFailed
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		message := fmt.Sprintf("unknown mode %q", exp.Spec.Mode)
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidMode", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidMode", message)
		return writeStatus(ctx, ri, name, &status)
	}
	serviceName := normalizeName(exp.Spec.ServiceRef)
	if serviceName == "" {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		message := "the Exposure has no serviceRef"
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "ServiceMissing", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "ServiceMissing", message)
		return writeStatus(ctx, ri, name, &status)
	}
	svc, err := readService(ctx, dyn, serviceName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		message := "the Service " + serviceName + " is not present in workspace " + c.workspace
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "ServiceMissing", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "ServiceMissing", message)
		return writeStatus(ctx, ri, name, &status)
	}
	if svc.Spec.Suspend {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		message := "the Service " + serviceName + " is suspended"
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "ServiceSuspended", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "ServiceSuspended", message)
		return writeStatus(ctx, ri, name, &status)
	}
	target, err := targetKey(svc.Spec.TargetRef, c.workspace)
	if err != nil {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposureFailed
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidTarget", err.Error())
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidTarget", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	handler, found, err := checkHandler(ctx, c.handlers, target, svc.Spec.Protocol, svc.Spec.Handler)
	if err != nil {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposureFailed
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidProtocol", err.Error())
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidProtocol", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	if !found {
		c.stop(name)
		c.release(name)
		status.Phase = exposurev1alpha1.ExposurePending
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		message := fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", handlerName(svc.Spec.Handler), target.Kind, target.Name, target.Workspace)
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "HandlerMissing", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "HandlerMissing", message)
		return writeStatus(ctx, ri, name, &status)
	}
	fingerprint, err := specFingerprint(&exp, svc, target)
	if err != nil {
		return err
	}
	handlerRevision := uint64(0)
	if c.handlers != nil {
		handlerRevision = c.handlers.HandlerRevision(target, svc.Spec.Handler)
	}
	fingerprint += fmt.Sprintf("|handler-%d", handlerRevision)
	if current := c.current(name); current != nil && current.fingerprint == fingerprint {
		return nil
	}
	c.stop(name)
	c.release(name)
	var url, endpoint string
	var rt *exposureRuntime
	switch exp.Spec.Mode {
	case exposurev1alpha1.ExposureModeLocal:
		rt, url, endpoint, err = c.startLocal(ctx, &exp, svc, target, handler)
	case exposurev1alpha1.ExposureModeIngress:
		rt, url, endpoint, err = c.startIngress(ctx, &exp, svc, target)
	}
	if err != nil {
		c.release(name)
		reason := "Pending"
		status.Phase = exposurev1alpha1.ExposurePending
		if errors.Is(err, ErrStableHostMismatch) {
			reason = "StableHostMismatch"
			status.Phase = exposurev1alpha1.ExposureFailed
		}
		status.Ready = false
		status.Address = ""
		status.Endpoint = ""
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", "the target handler is registered")
		setCondition(&status.Conditions, generation, ConditionReady, false, reason, err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	rt.fingerprint = fingerprint
	c.store(name, rt)
	status.Phase = exposurev1alpha1.ExposureReady
	status.Ready = true
	status.Address = url
	status.Endpoint = endpoint
	setCondition(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", "the target handler is registered")
	if rt.provider != "" {
		setCondition(&status.Conditions, generation, ConditionIngressProvider, true, "ProviderReady", rt.provider)
	}
	setCondition(&status.Conditions, generation, ConditionReady, true, "Ready", readyMessage(rt))
	return writeStatus(ctx, ri, name, &status)
}

func readyMessage(rt *exposureRuntime) string {
	if rt != nil && rt.provider != "" {
		return "the Exposure is bound by the " + rt.provider + " ingress provider"
	}
	return "the Exposure is bound"
}

func readService(ctx context.Context, dyn dynamic.Interface, name string) (*exposurev1alpha1.Service, error) {
	u, err := dyn.Resource(ServiceGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var svc exposurev1alpha1.Service
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &svc); err != nil {
		return nil, fmt.Errorf("exposure: decoding Service %s: %w", name, err)
	}
	return &svc, nil
}

func specFingerprint(exp *exposurev1alpha1.Exposure, svc *exposurev1alpha1.Service, target component.Key) (string, error) {
	value := map[string]any{
		"exposure": map[string]any{"spec": exp.Spec, "generation": exp.Generation},
		"service":  map[string]any{"spec": svc.Spec, "generation": svc.Generation},
		"target":   target,
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (c *ExposureController) current(name string) *exposureRuntime {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running[name]
}

func (c *ExposureController) store(name string, rt *exposureRuntime) {
	c.mu.Lock()
	c.running[name] = rt
	c.mu.Unlock()
}

func (c *ExposureController) stop(name string) {
	c.mu.Lock()
	rt := c.running[name]
	delete(c.running, name)
	c.mu.Unlock()
	rt.shutdown()
}

func (c *ExposureController) forget(name string) {
	c.stop(name)
	c.release(name)
}

func (c *ExposureController) release(name string) {
	if c.ports == nil {
		return
	}
	_ = c.ports.Release(leaseName(c.workspace, name))
}

func leaseName(workspace, name string) string {
	return "Exposure/" + workspace + "/" + name
}

func (c *ExposureController) startLocal(ctx context.Context, exp *exposurev1alpha1.Exposure, svc *exposurev1alpha1.Service, target component.Key, handler http.Handler) (*exposureRuntime, string, string, error) {
	if c.ports == nil {
		return nil, "", "", errors.New("the port registry is not configured")
	}
	listener, lease, err := c.ports.Listen(leaseName(c.workspace, exp.Name))
	if err != nil {
		return nil, "", "", err
	}
	tlsConfig, err := c.localTLS(exp.Spec.TLS)
	if err != nil {
		_ = listener.Close()
		return nil, "", "", err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	protocol := protocolKind(svc.Spec.Protocol)
	path := normalizePath(exp.Spec.Path)
	rt := &exposureRuntime{}
	switch protocol {
	case component.ProtocolHTTP, component.ProtocolWS:
		if handler == nil {
			_ = listener.Close()
			return nil, "", "", errors.New("the target HTTP handler is not registered")
		}
		current := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h, ok := c.handlers.HandlerNamed(target, handlerName(svc.Spec.Handler))
			if !ok {
				http.Error(w, "the target handler is not registered", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r.WithContext(component.WithCaller(r.Context(), target)))
		})
		server := &http.Server{Handler: current, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			_ = server.Serve(listener)
		}()
		rt.close = func() {
			_ = server.Close()
		}
		scheme := httpScheme(protocol, tlsConfig != nil)
		return rt, scheme + "://" + lease.Address() + path, lease.Address(), nil
	case component.ProtocolTCP, component.ProtocolSSH:
		runCtx, cancel := context.WithCancel(bus.WithCaller(context.Background(), ExposureCaller(c.workspace, exp.Name)))
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				go bridge(runCtx, c.handlers, target, svc.Spec.Handler, conn)
			}
		}()
		rt.close = func() {
			cancel()
			_ = listener.Close()
			<-done
		}
		scheme := protocol
		if scheme == "" {
			scheme = component.ProtocolTCP
		}
		return rt, scheme + "://" + lease.Address(), lease.Address(), nil
	default:
		_ = listener.Close()
		return nil, "", "", fmt.Errorf("unknown protocol %q", svc.Spec.Protocol)
	}
}

func (c *ExposureController) startIngress(ctx context.Context, exp *exposurev1alpha1.Exposure, svc *exposurev1alpha1.Service, target component.Key) (*exposureRuntime, string, string, error) {
	if c.registrar == nil {
		return nil, "", "", errors.New("no ingress registrar is configured")
	}
	registration, err := c.registrar.Register(ctx, IngressSpec{
		Workspace:      c.workspace,
		Exposure:       exp.Name,
		Service:        normalizeName(exp.Spec.ServiceRef),
		Protocol:       svc.Spec.Protocol,
		Handler:        svc.Spec.Handler,
		Target:         target,
		IngressRef:     exp.Spec.IngressRef,
		Hostname:       exp.Spec.Hostname,
		IngressAddress: exp.Spec.IngressAddress,
		IngressDial:    exp.Spec.IngressDial,
		PublicHost:     exp.Spec.PublicHost,
		Path:           normalizePath(exp.Spec.Path),
		TLS:            exp.Spec.TLS.Enabled,
		AuthRequired:   exp.Spec.Auth.Required,
		AllowedDIDs:    append([]string(nil), exp.Spec.Auth.AllowedDIDs...),
	})
	if err != nil {
		return nil, "", "", err
	}
	url := registration.URL()
	provider := ""
	if described, ok := registration.(IngressRegistrationProvider); ok {
		provider = strings.TrimSpace(described.Provider())
	}
	if path := normalizePath(exp.Spec.Path); path == "" || path == "/" {
		url = originURL(url)
	}
	rt := &exposureRuntime{provider: provider, close: func() { _ = registration.Close() }}
	return rt, url, url, nil
}

func originURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw
	}
	return parsed.Scheme + "://" + parsed.Host
}

func (c *ExposureController) localTLS(spec exposurev1alpha1.ExposureTLS) (*tls.Config, error) {
	if !spec.Enabled && strings.TrimSpace(spec.CertRef) == "" {
		return nil, nil
	}
	var config *tls.Config
	switch {
	case strings.TrimSpace(spec.CertRef) != "":
		certFile := strings.TrimSpace(spec.CertRef)
		reloader, err := tlsreload.New(certFile, []string{keyFileFor(certFile)}, c.log)
		if err != nil {
			return nil, fmt.Errorf("loading the certificate %s: %w", certFile, err)
		}
		config = reloader.TLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	case c.tls != nil:
		config = c.tls.Clone()
		if config.MinVersion == 0 {
			config.MinVersion = tls.VersionTLS12
		}
	default:
		return nil, errors.New("TLS is enabled but no certificate is available")
	}
	if caRef := strings.TrimSpace(spec.CABundleRef); caRef != "" {
		data, err := os.ReadFile(caRef)
		if err != nil {
			return nil, fmt.Errorf("reading the CA bundle %s: %w", caRef, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(data) {
			return nil, fmt.Errorf("the CA bundle %s holds no certificate", caRef)
		}
		config.ClientCAs = pool
		config.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return config, nil
}

func bridge(ctx context.Context, handlers *component.Handlers, target component.Key, name string, conn net.Conn) {
	defer conn.Close()
	if handlers == nil {
		return
	}
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

func keyFileFor(certFile string) string {
	switch {
	case certFile == "":
		return ""
	case strings.HasSuffix(certFile, ".crt"):
		return strings.TrimSuffix(certFile, ".crt") + ".key"
	case strings.HasSuffix(certFile, ".pem"):
		return strings.TrimSuffix(certFile, ".pem") + ".key"
	default:
		return certFile + ".key"
	}
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

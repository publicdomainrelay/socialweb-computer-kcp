package exposure

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/networkdial"
)

const relayMaxBody = 8 << 20

var ErrStableHostMismatch = errors.New("StableHostMismatch")

func ExposureCaller(workspace, name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: exposurev1alpha1.GroupVersion.Group, Kind: "Exposure", Name: name, Workspace: workspace}
}

func (s IngressSpec) Caller() commonv1.ObjectRef {
	return ExposureCaller(s.Workspace, s.Exposure)
}

type IngressSpec struct {
	Workspace string

	Exposure string

	Service string

	Protocol exposurev1alpha1.ServiceProtocol

	Handler string

	Target component.Key

	IngressRef string

	Hostname string

	IngressAddress string

	IngressDial string

	PublicHost string

	Path string

	TLS bool

	AuthRequired bool

	AllowedDIDs []string
}

type IngressRegistration interface {
	URL() string

	Close() error
}

type IngressRegistrationProvider interface {
	Provider() string
}

type IngressRegistrar interface {
	Register(ctx context.Context, spec IngressSpec) (IngressRegistration, error)
}

type IngressIdentity interface {
	Keypair(ctx context.Context, workspace, name string) (ingress.Keypair, error)

	ServiceAuthToken(ctx context.Context, workspace, name, aud, nsid string) (string, error)
}

type SubscriberRegistrar struct {
	deps     component.Deps
	identity IngressIdentity
}

func NewSubscriberRegistrar(deps component.Deps, identity IngressIdentity) *SubscriberRegistrar {
	return &SubscriberRegistrar{deps: deps, identity: identity}
}

func (r *SubscriberRegistrar) Register(ctx context.Context, spec IngressSpec) (IngressRegistration, error) {
	if r == nil || r.identity == nil {
		return nil, errors.New("no ingress identity is configured")
	}
	if r.deps.Registry == nil {
		return nil, errors.New("the component registry is not configured")
	}
	host, secure, err := r.proxyHost(spec)
	if err != nil {
		if address := strings.TrimSpace(spec.IngressAddress); address != "" {
			host, secure = address, false
		} else {
			return nil, err
		}
	}
	connectHost := host
	var httpClient *http.Client
	var wsDialer *websocket.Dialer
	if address := strings.TrimSpace(spec.IngressAddress); address != "" {
		connectHost, secure = address, ingressSecure(address)
	}
	audienceHost := hostport.URLHostname(connectHost)
	if dial := strings.TrimSpace(spec.IngressDial); dial != "" && connectHost != "" {
		hosts, err := networkdial.NewHostMap([]networkdial.Host{{Name: hostport.URLHostname(connectHost), Address: dial}})
		if err != nil {
			return nil, fmt.Errorf("the ingress dial address %q: %w", dial, err)
		}
		transport, err := networkdial.NewHTTPTransportWithOptions(networkdial.DialOptions{Hosts: hosts})
		if err != nil {
			return nil, err
		}
		httpClient = &http.Client{Transport: transport, Timeout: 30 * time.Second}
		if wsDialer, err = networkdial.NewWebSocketDialerWithOptions(networkdial.DialOptions{Hosts: hosts}); err != nil {
			return nil, err
		}
	}
	keypair, err := r.identity.Keypair(ctx, spec.Workspace, spec.Exposure)
	if err != nil {
		return nil, fmt.Errorf("resolving the ingress keypair: %w", err)
	}
	if keypair == nil {
		return nil, errors.New("the ingress identity returned no keypair")
	}
	did := keypair.DID()
	aud := "did:web:" + audienceHost
	getToken := func(ctx context.Context, nsid string) (string, error) {
		return r.identity.ServiceAuthToken(ctx, spec.Workspace, spec.Exposure, aud, nsid)
	}
	handler := r.handler(spec.Target, spec.Handler)
	tunnel := r.tunnel(spec.Target, spec.Handler, spec.Caller())
	wsTarget := r.wsTarget(spec.Target)
	subscriber, err := ingress.NewSubscriber(ctx, ingress.SubscriberConfig{
		Label:               "exposure-" + spec.Exposure,
		Keypair:             keypair,
		GetServiceAuthToken: getToken,
		IngressProxyHost:    connectHost,
		TLS:                 secure,
		HTTPClient:          httpClient,
		Dialer:              wsDialer,
		HandleRequest:       handler,
		TunnelHandler:       tunnel,
		WSTarget:            wsTarget,
		WSTargetDialer:      r.wsDialer(spec.Target, spec.Caller()),
	})
	if err != nil {
		return nil, fmt.Errorf("connecting the ingress subscriber: %w", err)
	}
	scheme := "https"
	publicHost := strings.TrimSpace(spec.PublicHost)
	if publicHost == "" {
		publicHost = hostport.URLHostname(strings.TrimSpace(spec.IngressAddress))
	}
	publicHost = cmp.Or(publicHost, host)
	urlHost := hostport.DIDLabel(did) + "." + publicHost
	path := normalizePath(spec.Path)
	raw := scheme + "://" + urlHost + path
	return &subscriberRegistration{url: raw, subscriber: subscriber}, nil
}

func (r *SubscriberRegistrar) proxyHost(spec IngressSpec) (string, bool, error) {
	name := strings.TrimSpace(spec.IngressRef)
	name = cmp.Or(name, DefaultIngressName)
	workspace := strings.TrimSpace(spec.Workspace)
	for _, endpointName := range []string{"control", "http", "https"} {
		endpoint, ok := r.deps.Registry.Endpoint(workspace, "IngressProxy", name, endpointName)
		if !ok {
			continue
		}
		if host, secure, ok := endpointHost(endpoint); ok {
			return host, secure, nil
		}
	}
	for _, entry := range r.deps.Registry.List("") {
		if entry.Key.Kind != "IngressProxy" || entry.Key.Name != name {
			continue
		}
		for _, endpoint := range entry.Endpoints {
			if host, secure, ok := endpointHost(endpoint); ok {
				return host, secure, nil
			}
		}
	}
	return "", false, fmt.Errorf("the IngressProxy component %s is not ready", name)
}

func ingressSecure(address string) bool {
	lower := strings.ToLower(strings.TrimSpace(address))
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "wss://"):
		return true
	case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "ws://"):
		return false
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return true
	}
	return port == "443"
}

func endpointHost(endpoint component.Endpoint) (string, bool, bool) {
	host := strings.TrimSpace(endpoint.Address)
	if host == "" && endpoint.URL != "" {
		if parsed, err := url.Parse(endpoint.URL); err == nil {
			host = parsed.Host
		}
	}
	if host == "" {
		return "", false, false
	}
	secure := strings.HasPrefix(strings.ToLower(endpoint.URL), "https://") ||
		strings.EqualFold(endpoint.Protocol, component.ProtocolHTTPS) ||
		strings.EqualFold(endpoint.Protocol, component.ProtocolWSS)
	return host, secure, true
}

func (r *SubscriberRegistrar) handler(target component.Key, name string) func(context.Context, *ingress.Request) (*ingress.Response, error) {
	return func(ctx context.Context, request *ingress.Request) (*ingress.Response, error) {
		if request == nil {
			return nil, errors.New("the ingress request is empty")
		}
		registry := r.deps.Handlers
		if registry == nil {
			return nil, errors.New("the handler registry is not configured")
		}
		handler, ok := registry.Handler(target, name)
		if !ok {
			return nil, fmt.Errorf("no HTTP handler %q is registered for %s/%s", handlerName(name), target.Kind, target.Name)
		}
		path := request.Path
		path = cmp.Or(path, "/")
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		if len(request.Params) > 0 {
			values := url.Values{}
			for key, value := range request.Params {
				values.Set(key, value)
			}
			path += "?" + values.Encode()
		}
		body := request.RawBody
		if body == "" && len(request.Body) > 0 {
			body = string(request.Body)
		}
		req, err := http.NewRequestWithContext(ctx, request.Method, "http://"+strings.ToLower(target.Kind+"."+target.Name)+path, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		for key, value := range request.Headers {
			req.Header.Set(key, value)
		}
		if host := strings.TrimSpace(request.Headers["host"]); host != "" {
			req.Host = host
		}
		if req.Host != "" {
			if req.Header.Get("X-Forwarded-Host") == "" {
				req.Header.Set("X-Forwarded-Host", req.Host)
			}
			if req.Header.Get("X-Forwarded-Proto") == "" {
				req.Header.Set("X-Forwarded-Proto", "https")
			}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req.WithContext(component.WithCaller(req.Context(), target)))
		result := recorder.Result()
		defer result.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(result.Body, relayMaxBody))
		if err != nil {
			return nil, err
		}
		headers := map[string]string{}
		if contentType := result.Header.Get("Content-Type"); contentType != "" {
			headers["content-type"] = contentType
		}
		return &ingress.Response{Status: result.StatusCode, Body: json.RawMessage(raw), ContentType: result.Header.Get("Content-Type"), Headers: headers}, nil
	}
}

func (r *SubscriberRegistrar) wsTarget(target component.Key) func() *ingress.Target {
	return func() *ingress.Target {
		if r == nil || r.deps.Registry == nil {
			return nil
		}
		address := ""
		for _, name := range []string{"http", "https", ""} {
			endpoint, ok := r.deps.Registry.Endpoint(target.Workspace, target.Kind, target.Name, name)
			if !ok {
				continue
			}
			address = strings.TrimSpace(endpoint.Address)
			if address == "" && endpoint.URL != "" {
				if parsed, err := url.Parse(endpoint.URL); err == nil {
					address = parsed.Host
				}
			}
			if address != "" {
				break
			}
		}
		if host, port, err := net.SplitHostPort(address); err == nil && host != "" && port != "" {
			value, err := strconv.Atoi(port)
			if err == nil && value > 0 {
				return &ingress.Target{Hostname: host, Port: value}
			}
		}
		if r.deps.Handlers == nil {
			return nil
		}
		host := ""
		for _, name := range []string{"http", "https", ""} {
			endpoint, ok := r.deps.Registry.Endpoint(target.Workspace, target.Kind, target.Name, name)
			if !ok || strings.TrimSpace(endpoint.URL) == "" {
				continue
			}
			if parsed, err := url.Parse(endpoint.URL); err == nil && parsed.Hostname() != "" {
				host = parsed.Hostname()
				break
			}
		}
		if host == "" {
			return nil
		}
		return &ingress.Target{Hostname: host, Port: 80}
	}
}

func (r *SubscriberRegistrar) wsDialer(target component.Key, caller commonv1.ObjectRef) *websocket.Dialer {
	if r == nil || r.deps.Registry == nil || r.deps.Handlers == nil {
		return nil
	}
	for _, name := range []string{"http", "https", ""} {
		endpoint, ok := r.deps.Registry.Endpoint(target.Workspace, target.Kind, target.Name, name)
		if !ok {
			continue
		}
		address := strings.TrimSpace(endpoint.Address)
		if address == "" && strings.TrimSpace(endpoint.URL) != "" {
			if parsed, err := url.Parse(endpoint.URL); err == nil {
				address = parsed.Host
			}
		}
		if host, port, err := net.SplitHostPort(address); err == nil && host != "" && port != "" {
			return nil
		}
		break
	}
	return &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		NetDialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return r.deps.Handlers.DialHTTP(bus.WithCaller(ctx, caller), address)
		},
	}
}

func (r *SubscriberRegistrar) tunnel(target component.Key, name string, caller commonv1.ObjectRef) func(context.Context, net.Conn) error {
	return func(ctx context.Context, conn net.Conn) error {
		if conn == nil {
			return errors.New("the tunnel connection is empty")
		}
		registry := r.deps.Handlers
		if registry == nil {
			return errors.New("the handler registry is not configured")
		}
		stream, err := registry.DialStream(bus.WithCaller(ctx, caller), target, name)
		if err != nil {
			return err
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
		return nil
	}
}

type subscriberRegistration struct {
	url        string
	subscriber *ingress.Subscriber
}

func (r *subscriberRegistration) URL() string {
	return r.url
}

func (r *subscriberRegistration) Provider() string {
	return "xrpc-subscription (https URL)"
}

func (r *subscriberRegistration) Close() error {
	if r == nil || r.subscriber == nil {
		return nil
	}
	r.subscriber.Close()
	return nil
}

package guestcontrolplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

func (s *Service) startRelay(ctx context.Context) error {
	if s.cfg.Relay == nil {
		return nil
	}
	relay := s.cfg.Relay
	keypair := relay.Keypair
	if keypair == nil {
		if s.cfg.StateDir == "" {
			return errors.New("guestcontrolplane: the relay names no keypair and there is no state dir")
		}
		signer, err := market.LoadOrCreateSignerFile(s.cfg.StateDir + "/guest-relay.jwk")
		if err != nil {
			return err
		}
		did, err := signer.PublicDID()
		if err != nil {
			return err
		}
		keypair = signerKeypair{signer: signer, did: did}
	}
	if relay.GetServiceAuthToken == nil {
		return errors.New("guestcontrolplane: the relay has no service-auth token source")
	}
	label := relay.Label
	if label == "" {
		label = "guest-" + s.cfg.Role
	}
	subscriber, err := ingress.NewSubscriber(ctx, ingress.SubscriberConfig{
		Label:               label,
		Keypair:             keypair,
		GetServiceAuthToken: relay.GetServiceAuthToken,
		IngressProxyHost:    relay.IngressProxyHost,
		TLS:                 relay.TLS,
		HandleRequest:       s.forwardRelay,
	})
	if err != nil {
		return err
	}
	s.relay = subscriber
	if endpoint, ok := s.relayEndpoint(); ok {
		s.addEndpoint(endpoint)
	}
	return nil
}

func (s *Service) relayEndpoint() (component.Endpoint, bool) {
	if s.relay == nil || s.cfg.Relay == nil {
		return component.Endpoint{}, false
	}
	host := s.cfg.Relay.IngressProxyHost
	if host == "" {
		return component.Endpoint{}, false
	}
	scheme := "https"
	if !s.cfg.Relay.TLS && (strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost")) {
		scheme = "http"
	}
	raw := scheme + "://" + s.relay.Subdomain() + "." + host
	protocol := component.ProtocolHTTPS
	if scheme == "http" {
		protocol = component.ProtocolHTTP
	}
	return component.Endpoint{Name: "relay", Protocol: protocol, URL: raw}, true
}

func (s *Service) relayURL() string {
	if endpoint, ok := s.relayEndpoint(); ok {
		return endpoint.URL
	}
	return ""
}

func (s *Service) forwardRelay(ctx context.Context, request *ingress.Request) (*ingress.Response, error) {
	if request == nil {
		return nil, errors.New("guestcontrolplane: the relay request is empty")
	}
	path := request.Path
	if path == "" {
		path = "/"
	}
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
	req, err := http.NewRequestWithContext(ctx, request.Method, "http://relay.local"+path, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	s.mux.ServeHTTP(recorder, req)
	result := recorder.Result()
	defer result.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(result.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return &ingress.Response{Status: result.StatusCode, Body: json.RawMessage(raw), ContentType: result.Header.Get("Content-Type")}, nil
}

type signerKeypair struct {
	signer *market.Signer

	did string
}

func (k signerKeypair) DID() string { return k.did }

func (k signerKeypair) Sign(data []byte) ([]byte, error) {
	return market.SignBytes(data, k.signer.PrivateKey)
}

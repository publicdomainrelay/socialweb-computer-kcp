package market

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	EndpointIroh = "iroh"

	EndpointWebRTC = "webrtc"

	EndpointHTTPS = "https"

	irohTicketPrefix = "endpoint"
)

func (e Endpoint) Validate() error {
	address := strings.TrimSpace(e.Address)
	if address == "" || address != e.Address || len(address) > 2048 {
		return fmt.Errorf("%w: endpoint %s has an empty, padded or oversized address", ErrInvalidRecord, e.Kind)
	}
	switch e.Kind {
	case EndpointIroh:
		body, ok := strings.CutPrefix(address, irohTicketPrefix)
		if !ok || body == "" || strings.Trim(body, "abcdefghijklmnopqrstuvwxyz234567") != "" {
			return fmt.Errorf("%w: iroh endpoint %q is not an endpoint ticket", ErrInvalidRecord, address)
		}
	case EndpointWebRTC:
		if _, err := ParseWebRTCAddress(address); err != nil {
			return err
		}
	case EndpointHTTPS:
		u, err := url.Parse(address)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
			return fmt.Errorf("%w: https endpoint %q is not an absolute http(s) URL", ErrInvalidRecord, address)
		}
	case "":
		return fmt.Errorf("%w: endpoint %q has no kind", ErrInvalidRecord, address)
	}
	return nil
}

func EndpointFor(address string) (Endpoint, error) {
	address = strings.TrimRight(strings.TrimSpace(address), "/")
	var out Endpoint
	if ticket, ok := strings.CutPrefix(address, EndpointIroh+"://"); ok {
		out = Endpoint{Kind: EndpointIroh, Address: ticket}
	} else {
		out = Endpoint{Kind: EndpointHTTPS, Address: address}
	}
	return out, out.Validate()
}

type WebRTCListen struct {
	DID string `json:"did"`

	Listen string `json:"listen"`
}

func ParseWebRTCAddress(address string) (WebRTCListen, error) {
	var out WebRTCListen
	raw, err := base64.RawURLEncoding.DecodeString(address)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil || !strings.HasPrefix(out.DID, "did:") || out.Listen == "" || strings.Contains(out.DID+out.Listen, "/") {
		return WebRTCListen{}, fmt.Errorf("%w: webrtc endpoint %q is not base64url {did, listen}", ErrInvalidRecord, address)
	}
	return out, nil
}

func (e Endpoint) URL() string {
	if e.Kind == EndpointIroh || e.Kind == EndpointWebRTC {
		return e.Kind + "://" + e.Address
	}
	return strings.TrimRight(e.Address, "/")
}

package ingress

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"
)

const (
	SubscribeNSID    = "com.fedproxy.temp.xrpc.subscribe"
	GetNonceNSID     = "com.fedproxy.temp.xrpc.getRegistrationNonce"
	TunnelNSID       = "com.fedproxy.temp.xrpc.tunnel"
	RegistrationType = "com.fedproxy.temp.xrpc.registration"
	KeepalivePath    = "/xrpc/com.publicdomainrelay.temp.keepalive.ping"
	DefaultServiceID = "xrpc_relay"
)

var relayedResponseHeaders = map[string]struct{}{
	"dpop-nonce":       {},
	"www-authenticate": {},
	"location":         {},
	"cache-control":    {},
	"retry-after":      {},
}

type Signature struct {
	Key       string `json:"key"`
	Signature string `json:"signature"`
}

type Registration struct {
	Type       string      `json:"$type"`
	Key        string      `json:"key"`
	Nonce      string      `json:"nonce"`
	Signatures []Signature `json:"signatures"`
}

type Request struct {
	RequestID string
	Method    string
	Path      string
	Params    map[string]string
	Body      json.RawMessage
	RawBody   string
	Headers   map[string]string
}

type Response struct {
	Status      int
	Body        json.RawMessage
	ContentType string
	Headers     map[string]string
}

type frame struct {
	Type           string            `json:"$type"`
	RequestID      string            `json:"requestId"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Params         map[string]string `json:"params"`
	Body           json.RawMessage   `json:"body"`
	RawBody        *string           `json:"rawBody"`
	Headers        map[string]string `json:"headers"`
	Status         int               `json:"status"`
	ContentType    string            `json:"contentType"`
	SubscriptionID string            `json:"subscriptionId"`
	NSID           string            `json:"nsid"`
	Message        json.RawMessage   `json:"message"`
	Data           string            `json:"data"`
	Code           int               `json:"code"`
	Reason         string            `json:"reason"`
	Key            string            `json:"key"`
	Nonce          string            `json:"nonce"`
	Signatures     []Signature       `json:"signatures"`
	Subdomain      string            `json:"subdomain"`
	IngressRef     string            `json:"ingressRef"`
}

func b64encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func b64decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func httpOrigin(host string, tls bool) string {
	if !tls && (strings.Contains(host, ":") || host == "localhost") {
		return "http://" + host
	}
	return "https://" + host
}

func wsOrigin(host string, tls bool) string {
	if !tls && (strings.Contains(host, ":") || host == "localhost") {
		return "ws://" + host
	}
	return "wss://" + host
}

func IngressURL(did, host string) string {
	return "https://" + hostport.DIDLabel(did) + "." + hostport.HostnameOnly(host)
}

func splitDataHost(host string, bases []string) (string, string, bool) {
	for _, base := range bases {
		if base == "" || base == "*" {
			continue
		}
		if before, ok := strings.CutSuffix(host, "."+base); ok {
			sub := before
			if sub == "" {
				continue
			}
			return base, sub, true
		}
	}
	return "", "", false
}

func frameType(kind string) string {
	return SubscribeNSID + "#" + kind
}

func kindOf(typeName string) string {
	prefix := SubscribeNSID + "#"
	if !strings.HasPrefix(typeName, prefix) {
		return ""
	}
	return strings.TrimPrefix(typeName, prefix)
}

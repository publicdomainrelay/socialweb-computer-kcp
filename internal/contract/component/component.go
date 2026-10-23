package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
	ProtocolWS    = "ws"
	ProtocolWSS   = "wss"
	ProtocolTCP   = "tcp"
	ProtocolSSH   = "ssh"
)

type Key struct {
	Workspace string
	Kind      string
	Name      string
}

func (k Key) Ref() commonv1.ObjectRef {
	return commonv1.ObjectRef{Workspace: k.Workspace, Kind: k.Kind, Name: k.Name}
}

func KeyOf(ref commonv1.ObjectRef) Key {
	return Key{Workspace: strings.TrimSpace(ref.Workspace), Kind: strings.TrimSpace(ref.Kind), Name: strings.TrimSpace(ref.Name)}
}

type Endpoint struct {
	Name     string
	Protocol string
	URL      string
	Address  string
}

func PrimaryEndpoint(endpoints []Endpoint) (Endpoint, bool) {
	for _, endpoint := range endpoints {
		if endpoint.Name == ProtocolHTTP {
			return endpoint, true
		}
	}
	for _, endpoint := range endpoints {
		if endpoint.URL != "" {
			return endpoint, true
		}
	}
	if len(endpoints) > 0 {
		return endpoints[0], true
	}
	return Endpoint{}, false
}

type Service[D any] interface {
	Start(ctx context.Context, deps D) error
	Ready(ctx context.Context) error
	Endpoints() []Endpoint
	Stop(ctx context.Context) error
}

type NetworkHosts interface {
	Address(hostport string) (string, bool)
	Hosts() map[string]string
}

type RawSpec json.RawMessage

func (s RawSpec) Bytes() []byte {
	return []byte(s)
}

func (s RawSpec) Decode(v any) error {
	if len(s) == 0 {
		return nil
	}
	if err := json.Unmarshal(s, v); err != nil {
		return fmt.Errorf("component: decoding the raw spec: %w", err)
	}
	return nil
}

func (s RawSpec) Map() (map[string]any, error) {
	out := map[string]any{}
	if err := s.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func (s RawSpec) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(s) {
		return nil, errors.New("component: the raw spec is not valid JSON")
	}
	return s, nil
}

func (s *RawSpec) UnmarshalJSON(b []byte) error {
	if s == nil {
		return errors.New("component: raw spec unmarshal into nil")
	}
	*s = append((*s)[:0], b...)
	return nil
}

type RestartPolicy string

const (
	RestartAlways    RestartPolicy = "Always"
	RestartOnFailure RestartPolicy = "OnFailure"
	RestartNever     RestartPolicy = "Never"
)

func (p RestartPolicy) Valid() bool {
	switch p {
	case RestartAlways, RestartOnFailure, RestartNever:
		return true
	default:
		return false
	}
}

func (p RestartPolicy) WithDefault() RestartPolicy {
	if p.Valid() {
		return p
	}
	return RestartAlways
}

type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseStarting  Phase = "Starting"
	PhaseRunning   Phase = "Running"
	PhaseReady     Phase = "Ready"
	PhaseFailed    Phase = "Failed"
	PhaseStopping  Phase = "Stopping"
	PhaseSuspended Phase = "Suspended"
)

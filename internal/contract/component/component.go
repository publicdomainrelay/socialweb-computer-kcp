package component

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

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

type Service interface {
	Start(ctx context.Context) error
	Ready() Readiness
	Endpoints() []Endpoint
	Stop(ctx context.Context) error
}

type Readiness interface {
	Done() <-chan struct{}
	Err() error
}

var ErrNotReady = errors.New("component: the service failed to become ready")

type ReadySignal struct {
	mu   sync.Mutex
	done chan struct{}
	err  error
	set  bool
}

var _ Readiness = (*ReadySignal)(nil)

func (r *ReadySignal) Done() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.doneLocked()
}

func (r *ReadySignal) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *ReadySignal) Signal() {
	r.resolve(nil)
}

func (r *ReadySignal) Fail(err error) {
	if err == nil {
		err = ErrNotReady
	}
	r.resolve(err)
}

func (r *ReadySignal) resolve(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set {
		return
	}
	r.set = true
	r.err = err
	close(r.doneLocked())
}

func (r *ReadySignal) doneLocked() chan struct{} {
	if r.done == nil {
		r.done = make(chan struct{})
	}
	return r.done
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

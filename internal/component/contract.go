package component

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	AnnotationRestartAt = "component.socialweb.computer/restart-at"

	ConditionReady = "Ready"
)

const (
	ProtocolHTTP  = "http"
	ProtocolHTTPS = "https"
	ProtocolWS    = "ws"
	ProtocolWSS   = "wss"
	ProtocolTCP   = "tcp"
	ProtocolSSH   = "ssh"
)

type Service interface {
	Start(ctx context.Context, deps Deps) error
	Ready(ctx context.Context) error
	Endpoints() []Endpoint
	Stop(ctx context.Context) error
}

type Deps struct {
	KCP         KCPClients
	Credentials CredentialResolver
	Vault       VaultClients
	TLS         *tls.Config
	Log         *slog.Logger
	StateDir    string
	Hosts       NetworkHosts
	Registry    *Registry
	Handlers    *HandlerRegistry
}

type NetworkHosts interface {
	Address(hostport string) (string, bool)
	Hosts() map[string]string
}

type Endpoint struct {
	Name     string
	Protocol string
	URL      string
	Address  string
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

type ComponentSpec struct {
	UID           types.UID
	Kind          string
	Name          string
	Namespace     string
	Workspace     string
	Generation    int64
	Suspend       bool
	Deleting      bool
	RestartPolicy RestartPolicy
	StateDir      string
	Spec          RawSpec
	Annotations   map[string]string
}

func (s ComponentSpec) Check() error {
	switch {
	case s.UID == "":
		return fmt.Errorf("component: the %s/%s spec has no uid", s.Kind, s.Name)
	case s.Kind == "":
		return errors.New("component: the spec has no kind")
	case s.Name == "":
		return fmt.Errorf("component: the %s spec has no name", s.Kind)
	}
	return nil
}

func (s ComponentSpec) RestartStamp() string {
	return s.Annotations[AnnotationRestartAt]
}

func (s ComponentSpec) ComponentStateDir(base string) string {
	if s.StateDir != "" {
		return s.StateDir
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "components", s.Kind+"-"+s.Name)
}

type ComponentPhase string

const (
	PhasePending   ComponentPhase = "Pending"
	PhaseStarting  ComponentPhase = "Starting"
	PhaseRunning   ComponentPhase = "Running"
	PhaseReady     ComponentPhase = "Ready"
	PhaseFailed    ComponentPhase = "Failed"
	PhaseStopping  ComponentPhase = "Stopping"
	PhaseSuspended ComponentPhase = "Suspended"
)

type ComponentStatus struct {
	ObservedGeneration int64
	Phase              ComponentPhase
	Ready              bool
	Message            string
	Endpoints          []Endpoint
	Restarts           int32
	StartedAt          time.Time
	Conditions         []metav1.Condition
}

func (s ComponentStatus) Endpoint(name string) (Endpoint, bool) {
	for _, e := range s.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return Endpoint{}, false
}

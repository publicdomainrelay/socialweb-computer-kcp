package component

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

const (
	AnnotationRestartAt = "component.socialweb.computer/restart-at"

	ConditionReady = "Ready"
)

const (
	ProtocolHTTP  = contract.ProtocolHTTP
	ProtocolHTTPS = contract.ProtocolHTTPS
	ProtocolWS    = contract.ProtocolWS
	ProtocolWSS   = contract.ProtocolWSS
	ProtocolTCP   = contract.ProtocolTCP
	ProtocolSSH   = contract.ProtocolSSH
)

type Service = contract.Service[Deps]

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

	NodeClaimAgent *kcpclient.ServiceAccountRef
}

type NetworkHosts = contract.NetworkHosts

type Endpoint = contract.Endpoint

type RawSpec = contract.RawSpec

type RestartPolicy = contract.RestartPolicy

const (
	RestartAlways    = contract.RestartAlways
	RestartOnFailure = contract.RestartOnFailure
	RestartNever     = contract.RestartNever
)

type ComponentSpec struct {
	UID           types.UID
	Group         string
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
	Labels        map[string]string
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

type ComponentPhase = contract.Phase

const (
	PhasePending   = contract.PhasePending
	PhaseStarting  = contract.PhaseStarting
	PhaseRunning   = contract.PhaseRunning
	PhaseReady     = contract.PhaseReady
	PhaseFailed    = contract.PhaseFailed
	PhaseStopping  = contract.PhaseStopping
	PhaseSuspended = contract.PhaseSuspended
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

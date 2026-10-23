package guestcontrolplane

import (
	"cmp"
	"errors"
	"log/slog"
	"strings"
	"time"

	"k8s.io/utils/ptr"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const Kind = "GuestControlPlane"

const (
	DefaultOpenBaoTimeout = 10 * time.Second

	kindPds = "Pds"

	kindRemotePDS = "RemotePDS"
)

type Deps struct {
	component.Instance

	Hosts component.NetworkHosts
}

func New(spec v1alpha1.GuestControlPlaneSpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:        spec,
		deps:        deps,
		stateDir:    deps.StateDir,
		grantSync:   time.Duration(ptr.Deref(spec.GrantSyncSeconds, 0)) * time.Second,
		issuerRetry: time.Duration(ptr.Deref(spec.IssuerRetrySeconds, 0)) * time.Second,
	}
	if spec.Storage != nil && spec.Storage.DataDir != "" {
		s.stateDir = spec.Storage.DataDir
	}
	if s.handle() == "" {
		return nil, errors.New("guestcontrolplane: the component names no bidder handle")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s.log = log.With("component", "guestcontrolplane", "handle", s.handle())
	return s, nil
}

func (s *Service) network() v1alpha1.MarketNetworkValue {
	if s.spec.Network == nil {
		return v1alpha1.MarketNetworkValue{}
	}
	return *s.spec.Network
}

func (s *Service) workspace() string {
	return cmp.Or(s.deps.Self.Workspace, "root")
}

func (s *Service) self() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace()}
}

func (s *Service) handle() string {
	if s.spec.Identity == nil {
		return ""
	}
	return s.spec.Identity.Handle
}

func (s *Service) did() string {
	if s.spec.Identity == nil {
		return ""
	}
	return strings.TrimSpace(s.spec.Identity.Did)
}

func (s *Service) pdsRef() (commonv1.ObjectRef, error) {
	ref := s.spec.PdsRef
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}, errors.New("guestcontrolplane: the relay names no pdsRef to mint service auth with")
	}
	out := commonv1.ObjectRef{Group: strings.TrimSpace(ref.Group), Kind: cmp.Or(strings.TrimSpace(ref.Kind), kindPds),
		Name: strings.TrimSpace(ref.Name), Workspace: cmp.Or(strings.TrimSpace(ref.Workspace), s.workspace())}
	switch {
	case strings.EqualFold(out.Kind, kindPds):
		out.Kind, out.Group = kindPds, cmp.Or(out.Group, v1alpha1.GroupName)
	case strings.EqualFold(out.Kind, kindRemotePDS):
		out.Kind, out.Group = kindRemotePDS, cmp.Or(out.Group, pdsv1alpha1.GroupName)
	default:
		return commonv1.ObjectRef{}, errors.New("guestcontrolplane: the pdsRef names neither a Pds nor a RemotePDS")
	}
	return out, nil
}

func (s *Service) relayHost() string {
	return strings.TrimSpace(s.network().IngressProxyHost)
}

func (s *Service) relayTLS() bool {
	network := s.network()
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(network.GuestTLSURL())), "https://")
}

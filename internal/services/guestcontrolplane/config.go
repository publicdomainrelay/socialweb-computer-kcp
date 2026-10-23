package guestcontrolplane

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"k8s.io/utils/ptr"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const Kind = "GuestControlPlane"

const (
	DefaultVaultServer = "bidder"

	DefaultVaultClient = "guest-kcp"

	DefaultOpenBaoTimeout = 10 * time.Second
)

type Deps struct {
	component.Instance

	KCP component.KCPClients

	Hosts component.NetworkHosts

	RelayServiceAuth func(ctx context.Context, nsid string) (string, error)
}

func New(spec v1alpha1.GuestControlPlaneSpec, deps Deps) (*Service, error) {
	s := &Service{
		spec:        spec,
		deps:        deps,
		stateDir:    deps.StateDir,
		vaultServer: v1alpha1.VaultServer(spec.VaultRef, DefaultVaultServer),
		grantSync:   time.Duration(ptr.Deref(spec.GrantSyncSeconds, 0)) * time.Second,
		issuerRetry: time.Duration(ptr.Deref(spec.IssuerRetrySeconds, 0)) * time.Second,
	}
	if spec.Storage != nil && spec.Storage.DataDir != "" {
		s.stateDir = spec.Storage.DataDir
	}
	switch {
	case s.handle() == "":
		return nil, errors.New("guestcontrolplane: the component names no bidder handle")
	case deps.KCP == nil:
		return nil, errors.New("guestcontrolplane: the component has no OpenBao endpoint source")
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

func (s *Service) handle() string {
	if s.spec.Identity == nil {
		return ""
	}
	return s.spec.Identity.Handle
}

func (s *Service) identityRef() *commonv1.ObjectRef {
	if s.spec.Identity != nil {
		return s.spec.Identity.MarketIdentityRef
	}
	return s.spec.PdsRef
}

func (s *Service) relayHost() string {
	return strings.TrimSpace(s.network().IngressProxyHost)
}

func (s *Service) relayTLS() bool {
	network := s.network()
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(network.GuestTLSURL())), "https://")
}

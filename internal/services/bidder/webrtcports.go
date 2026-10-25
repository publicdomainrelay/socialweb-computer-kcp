package bidder

import (
	"fmt"
	"path/filepath"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
)

const webrtcPortsDir = "webrtc-udp-ports"

type webrtcPorts struct {
	reg *component.PortRegistry
}

var _ core.GuestUDPPorts = webrtcPorts{}

// ponytail: one webrtc UDP port registry per bidder state dir; two bidders on one host share the range only through the bind probe; upgrade to a host-wide registry when one host runs several bidders with webrtc guests
func (s *Service) webrtcPorts() (core.GuestUDPPorts, error) {
	ports := s.spec.GuestNetwork.WebRTCUDPPortRange()
	if ports.Min < guestnet.HighUDPMinPort || ports.Max > 65535 || ports.Max < ports.Min {
		return nil, fmt.Errorf("bidder: spec.guestNetwork.webrtcUDPPorts %d-%d is not a range from %d to 65535", ports.Min, ports.Max, guestnet.HighUDPMinPort)
	}
	reg, err := component.OpenPortRegistry(component.PortRegistryOptions{StateDir: filepath.Join(s.stateDir(), webrtcPortsDir),
		First: int(ports.Min), Last: int(ports.Max), Host: "0.0.0.0", Network: "udp"})
	if err != nil {
		return nil, fmt.Errorf("bidder: the webrtc UDP port registry: %w", err)
	}
	return webrtcPorts{reg: reg}, nil
}

func (w webrtcPorts) Allocate(name string) (int, error) {
	lease, err := w.reg.Allocate(name)
	return lease.Port, err
}

func (w webrtcPorts) Release(name string) error { return w.reg.Release(name) }

func (w webrtcPorts) Held() []string {
	leases := w.reg.Leases()
	out := make([]string, 0, len(leases))
	for _, l := range leases {
		out = append(out, l.Name)
	}
	return out
}

func (s *Service) lanAddresses() []string {
	out, err := guestnet.HostLANAddresses()
	if err != nil {
		s.log.Warn("the bidder cannot list the host's LAN addresses; the guest's webrtc gets no forwarded LAN UDP port", "err", err)
		return nil
	}
	return out
}

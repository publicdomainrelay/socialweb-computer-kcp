package requester

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const maxNetworkAddress = 2048

var (
	networkTagRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

	dtlsFingerprintRE = regexp.MustCompile(`^sha-256 [0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){31}$`)
)

func tunnelsOptions(contract v1alpha1.ComputeContract) (cloudinit.TunnelsOptions, error) {
	opts := cloudinit.TunnelsOptions{}
	for _, t := range contract.Spec.Tunnels {
		opts.Tunnels = append(opts.Tunnels, cloudinit.Tunnel{Tag: t.Tag, Port: t.Port, Server: t.Server, Transport: string(t.Transport)})
	}
	if err := opts.Validate(); err != nil {
		return opts, err
	}
	return opts, nil
}

func ValidateNetworkAddress(kind, tag, address string) error {
	if !networkTagRE.MatchString(tag) {
		return fmt.Errorf("requester: tag %q is not a DNS label of at most 32 characters", tag)
	}
	if len(address) > maxNetworkAddress {
		return fmt.Errorf("requester: the %s address is longer than %d bytes", tag, maxNetworkAddress)
	}
	switch kind {
	case market.OnNetworkKindHTTPS:
		u, err := url.Parse(address)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return fmt.Errorf("requester: the %s address %q is not an https URL", tag, address)
		}
	case market.OnNetworkKindWebRTC:
		if !dtlsFingerprintRE.MatchString(address) {
			return fmt.Errorf("requester: the %s address %q is not a DTLS fingerprint \"sha-256 XX:XX:...\"", tag, address)
		}
	default:
		return fmt.Errorf("requester: the %s address names kind %q; a tagged address is https or webrtc", tag, kind)
	}
	return nil
}

func recordNetwork(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	if state == nil || parsed == nil || parsed.Tag == "" {
		return false
	}
	next := v1alpha1.ContractNetworkAddress{Tag: parsed.Tag, Kind: v1alpha1.NetworkKind(parsed.NetworkKind), Address: parsed.Address,
		At: parsed.ObservedAt, Event: commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID}}
	i := slices.IndexFunc(state.Networks, func(n v1alpha1.ContractNetworkAddress) bool { return n.Tag == parsed.Tag })
	if i < 0 {
		state.Networks = append(state.Networks, next)
		return true
	}
	current := state.Networks[i]
	if current.Address == next.Address && current.Kind == next.Kind && current.Event.URI == next.Event.URI {
		return false
	}
	state.Networks[i] = next
	return true
}

func networkRecorded(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	if state == nil || parsed == nil {
		return false
	}
	for _, n := range state.Networks {
		if n.Tag == parsed.Tag && n.Address == parsed.Address && string(n.Kind) == parsed.NetworkKind {
			return true
		}
	}
	return false
}

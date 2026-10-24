package requester

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
)

const (
	opentunnelAsset = "opentunnel"

	woottyDownload = "wootty"

	maxNetworkAddress = 2048
)

var (
	networkTagRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

	guestArch = map[string]string{"linux/amd64": "x86_64", "linux/arm64": "aarch64"}

	errShellWithoutSecrets = errors.New("a shell needs its token in spec.secretsRef: the secrets bundle writes the shell's envFile on the guest")
)

func tunnelsOptions(contract v1alpha1.ComputeContract) (cloudinit.TunnelsOptions, error) {
	opts := cloudinit.TunnelsOptions{}
	shell := false
	for _, t := range contract.Spec.Tunnels {
		tunnel := cloudinit.Tunnel{Tag: t.Tag, Port: t.Port, Server: t.Server}
		if t.Shell != nil {
			tunnel.ShellEnvFile = strings.TrimSpace(t.Shell.EnvFile)
			if tunnel.ShellEnvFile == "" {
				tunnel.ShellEnvFile = cloudinit.DefaultShellEnvFile
			}
			shell = true
		}
		opts.Tunnels = append(opts.Tunnels, tunnel)
	}
	if shell && contract.Spec.SecretsRef == nil {
		return opts, errShellWithoutSecrets
	}
	fetches, err := versions.AssetFetches(opentunnelAsset)
	if err != nil {
		return opts, err
	}
	for _, platform := range sortedPlatforms(fetches) {
		f := fetches[platform]
		arch, ok := guestArch[platform]
		if !ok {
			continue
		}
		opts.OpenTunnel = append(opts.OpenTunnel, cloudinit.GuestBinaryPin{Arch: arch, URL: f.URL, SHA256: f.SHA256,
			Member: f.Members[opentunnelAsset]})
	}
	if shell {
		wootty, err := versions.GuestDownloadPins(woottyDownload)
		if err != nil {
			return opts, err
		}
		for _, platform := range sortedPlatforms(wootty.Fetch) {
			f := wootty.Fetch[platform]
			arch, ok := guestArch[platform]
			if !ok || f.Kind != "oci-layer" {
				continue
			}
			opts.Wootty = append(opts.Wootty, cloudinit.GuestBinaryPin{Arch: arch, Registry: f.Registry,
				Repository: f.Repository, SHA256: f.SHA256, Member: f.Member})
		}
	}
	if err := opts.Validate(); err != nil {
		return opts, err
	}
	return opts, nil
}

func sortedPlatforms[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func ValidateNetworkAddress(tag, address string) error {
	if !networkTagRE.MatchString(tag) {
		return fmt.Errorf("requester: tag %q is not a DNS label of at most 32 characters", tag)
	}
	if len(address) > maxNetworkAddress {
		return fmt.Errorf("requester: the %s address is longer than %d bytes", tag, maxNetworkAddress)
	}
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("requester: the %s address %q is not an https URL", tag, address)
	}
	return nil
}

func recordNetwork(state *v1alpha1.NodeClaimContract, parsed *GuestEvent) bool {
	if state == nil || parsed == nil || parsed.Tag == "" {
		return false
	}
	next := v1alpha1.ContractNetworkAddress{Tag: parsed.Tag, Address: parsed.Address, At: parsed.ObservedAt,
		Event: commonv1.StrongRef{URI: parsed.Event.URI, CID: parsed.Event.CID}}
	i := slices.IndexFunc(state.Networks, func(n v1alpha1.ContractNetworkAddress) bool { return n.Tag == parsed.Tag })
	if i < 0 {
		state.Networks = append(state.Networks, next)
		return true
	}
	if state.Networks[i].Address == next.Address && state.Networks[i].Event.URI == next.Event.URI {
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
		if n.Tag == parsed.Tag && n.Address == parsed.Address {
			return true
		}
	}
	return false
}

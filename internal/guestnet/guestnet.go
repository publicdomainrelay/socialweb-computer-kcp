package guestnet

import (
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

const (
	PastaAddress = "172.30.0.0"
	Gateway      = "172.30.0.1"
	GuestAddress = "172.30.0.2"
	Prefix       = 30
	GuestMAC     = "06:00:ac:1e:00:02"
	HostAddress  = "192.0.2.1"
	Tap          = "fctap0"
	Bridge       = "br0"
	Interface    = "eth0"
	Shell        = "/bin/sh"
	GuestLink    = "172.30.0.0/30"
	DNSPort      = 53

	LANUDPMinPort = 1024
)

var (
	BlockedPrefixes = []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/4", "255.255.255.255/32",
		"::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
	}

	LANUDPPrefixes = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

	LANUDPClosedPorts = []int{1900, 5353, 5355}
)

type Wiring struct {
	Tap          string `json:"tap"`
	Bridge       string `json:"bridge"`
	GuestAddress string `json:"guestAddress"`
	GuestMAC     string `json:"guestMAC"`
	HostPorts    []int  `json:"hostPorts,omitempty"`

	LANUDP        bool     `json:"lanUDP"`
	HostAddresses []string `json:"hostAddresses"`
}

type VMExec struct {
	Wiring  Wiring   `json:"wiring"`
	VMM     string   `json:"vmm"`
	Argv    []string `json:"argv"`
	Console string   `json:"console"`
}

func Plan(hostPorts []int, lanUDP bool, hostAddresses []string) Wiring {
	return Wiring{Tap: Tap, Bridge: Bridge, GuestAddress: GuestAddress, GuestMAC: GuestMAC, HostPorts: HostPorts(hostPorts),
		LANUDP: lanUDP, HostAddresses: slices.Clone(hostAddresses)}
}

func HostAddresses() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("guestnet: listing the host's addresses: %w", err)
	}
	var out []string
	for _, a := range addrs {
		p, err := netip.ParsePrefix(a.String())
		if err != nil {
			return nil, fmt.Errorf("guestnet: the host address %q: %w", a.String(), err)
		}
		if s := p.Addr().Unmap().String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("guestnet: the host has no address to fence the guest off")
	}
	slices.Sort(out)
	return out, nil
}

func ParsePrefixes(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("guestnet: %q is neither an address nor a prefix", s)
			}
			a = a.Unmap()
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func HostPorts(ports []int) []int {
	var out []int
	for _, p := range ports {
		if p > 0 && p <= 65535 && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func Netmask(prefix int) string {
	var mask uint32
	if prefix > 0 && prefix <= 32 {
		mask = ^uint32(0) << uint(32-prefix)
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

func PastaArgv(pasta string, child ...string) []string {
	argv := []string{
		pasta, "--config-net", "-q", "-f",
		"-a", PastaAddress,
		"-n", Netmask(Prefix),
		"-g", Gateway,
		"--dns-forward", Gateway,
		"--map-host-loopback", HostAddress,
		"--no-map-gw",
		"-t", "none", "-u", "none", "-T", "none", "-U", "none",
		"--",
		Shell, "-c", `exec "$0" "$@"`,
	}
	return append(argv, child...)
}

func NetworkConfig() string {
	lines := []string{
		"version: 2",
		"ethernets:",
		"  " + Interface + ":",
		"    match:",
		"      macaddress: \"" + GuestMAC + "\"",
		"    set-name: " + Interface,
		"    dhcp4: false",
		"    dhcp6: false",
		"    addresses: [" + GuestAddress + "/" + strconv.Itoa(Prefix) + "]",
		"    routes:",
		"      - to: default",
		"        via: " + Gateway,
		"    nameservers:",
		"      addresses: [" + Gateway + "]",
	}
	return strings.Join(lines, "\n") + "\n"
}

func (w Wiring) Validate() error {
	if w.Tap == "" || w.Bridge == "" {
		return fmt.Errorf("guestnet: the wiring names tap %q and bridge %q; both are needed", w.Tap, w.Bridge)
	}
	if _, err := netip.ParseAddr(w.GuestAddress); err != nil {
		return fmt.Errorf("guestnet: guest address %q: %w", w.GuestAddress, err)
	}
	if _, err := ParseMAC(w.GuestMAC); err != nil {
		return err
	}
	for _, p := range w.HostPorts {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("guestnet: host port %d is not a TCP port", p)
		}
	}
	if len(w.HostAddresses) == 0 {
		return fmt.Errorf("guestnet: the wiring names none of the host's addresses, so the fence could not keep the guest off them")
	}
	if _, err := ParsePrefixes(w.HostAddresses); err != nil {
		return err
	}
	return nil
}

func ParseMAC(s string) ([6]byte, error) {
	var mac [6]byte
	parts := strings.Split(s, ":")
	if len(parts) != 6 {
		return mac, fmt.Errorf("guestnet: %q is not a MAC address", s)
	}
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return mac, fmt.Errorf("guestnet: %q is not a MAC address", s)
		}
		mac[i] = byte(v)
	}
	return mac, nil
}

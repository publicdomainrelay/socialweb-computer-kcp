package guestnet

import (
	"fmt"
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
)

type Wiring struct {
	Tap          string `json:"tap"`
	Bridge       string `json:"bridge"`
	GuestAddress string `json:"guestAddress"`
	GuestMAC     string `json:"guestMAC"`
	HostPorts    []int  `json:"hostPorts,omitempty"`
}

type VMExec struct {
	Wiring  Wiring   `json:"wiring"`
	VMM     string   `json:"vmm"`
	Argv    []string `json:"argv"`
	Console string   `json:"console"`
}

func Plan(hostPorts []int) Wiring {
	return Wiring{Tap: Tap, Bridge: Bridge, GuestAddress: GuestAddress, GuestMAC: GuestMAC, HostPorts: HostPorts(hostPorts)}
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

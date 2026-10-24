//go:build linux

package guestnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func Wire(w Wiring) error {
	if err := w.Validate(); err != nil {
		return err
	}
	if err := Fence(w); err != nil {
		return err
	}
	uplink, err := pastaLink()
	if err != nil {
		return err
	}
	if err := createTap(w.Tap); err != nil {
		return err
	}
	nl, err := openNetlink()
	if err != nil {
		return err
	}
	defer nl.close()
	if err := nl.newBridge(w.Bridge); err != nil {
		return err
	}
	tap, err := net.InterfaceByName(w.Tap)
	if err != nil {
		return fmt.Errorf("guestnet: the tap %s: %w", w.Tap, err)
	}
	br, err := net.InterfaceByName(w.Bridge)
	if err != nil {
		return fmt.Errorf("guestnet: the bridge %s: %w", w.Bridge, err)
	}
	for _, step := range []struct {
		what string
		run  func() error
	}{
		{"bring up " + w.Bridge, func() error { return nl.setLink(br.Index, 0, true) }},
		{"bring up " + w.Tap, func() error { return nl.setLink(tap.Index, 0, true) }},
		{"enslave " + uplink.Name + " to " + w.Bridge, func() error { return nl.setLink(uplink.Index, br.Index, true) }},
		{"enslave " + w.Tap + " to " + w.Bridge, func() error { return nl.setLink(tap.Index, br.Index, true) }},
		{"pin the guest's neighbor entry", func() error { return nl.permanentNeighbor(br.Index, w.GuestAddress, w.GuestMAC) }},
		{"route the guest through " + w.Bridge, func() error { return nl.hostRoute(br.Index, w.GuestAddress) }},
	} {
		if err := step.run(); err != nil {
			return fmt.Errorf("guestnet: %s: %w", step.what, err)
		}
	}
	return nil
}

func pastaLink() (net.Interface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, fmt.Errorf("guestnet: listing this namespace's links: %w", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
				return i, nil
			}
		}
	}
	return net.Interface{}, fmt.Errorf("guestnet: no link but loopback carries an IPv4 address, so pasta has not configured this namespace and nothing is wired")
}

func createTap(name string) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("guestnet: opening /dev/net/tun: %w", err)
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return fmt.Errorf("guestnet: tap name %q: %w", name, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("guestnet: TUNSETIFF %s: %w", name, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETPERSIST, 1); err != nil {
		return fmt.Errorf("guestnet: TUNSETPERSIST %s: %w", name, err)
	}
	return nil
}

type netlink struct {
	fd  int
	seq uint32
}

func openNetlink() (*netlink, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("guestnet: netlink socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("guestnet: netlink bind: %w", err)
	}
	return &netlink{fd: fd}, nil
}

func (n *netlink) close() { unix.Close(n.fd) }

type attr struct {
	typ  uint16
	data []byte
}

func align4(n int) int { return (n + 3) &^ 3 }

func encodeAttrs(attrs []attr) []byte {
	var out []byte
	for _, a := range attrs {
		l := 4 + len(a.data)
		b := make([]byte, align4(l))
		binary.NativeEndian.PutUint16(b[0:], uint16(l))
		binary.NativeEndian.PutUint16(b[2:], a.typ)
		copy(b[4:], a.data)
		out = append(out, b...)
	}
	return out
}

func cstr(s string) []byte { return append([]byte(s), 0) }

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.NativeEndian.PutUint32(b, v)
	return b
}

func (n *netlink) do(typ uint16, flags uint16, body []byte) error {
	n.seq++
	hdr := unix.NlMsghdr{
		Len:   uint32(unix.SizeofNlMsghdr + len(body)),
		Type:  typ,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | flags,
		Seq:   n.seq,
	}
	msg := make([]byte, unix.SizeofNlMsghdr, int(hdr.Len))
	*(*unix.NlMsghdr)(unsafe.Pointer(&msg[0])) = hdr
	msg = append(msg, body...)
	if err := unix.Sendto(n.fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	buf := make([]byte, os.Getpagesize())
	for {
		nr, _, err := unix.Recvfrom(n.fd, buf, 0)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseNetlinkMessage(buf[:nr])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if m.Header.Seq != n.seq || m.Header.Type != unix.NLMSG_ERROR {
				continue
			}
			if len(m.Data) < 4 {
				return fmt.Errorf("short netlink error message")
			}
			if code := int32(binary.NativeEndian.Uint32(m.Data[:4])); code != 0 {
				return unix.Errno(-code)
			}
			return nil
		}
	}
}

func ifinfo(index int, flags, change uint32) []byte {
	b := make([]byte, unix.SizeofIfInfomsg)
	*(*unix.IfInfomsg)(unsafe.Pointer(&b[0])) = unix.IfInfomsg{Family: unix.AF_UNSPEC, Index: int32(index), Flags: flags, Change: change}
	return b
}

func (n *netlink) newBridge(name string) error {
	info := encodeAttrs([]attr{{unix.IFLA_INFO_KIND, cstr("bridge")}})
	body := append(ifinfo(0, 0, 0), encodeAttrs([]attr{
		{unix.IFLA_IFNAME, cstr(name)},
		{unix.IFLA_LINKINFO | unix.NLA_F_NESTED, info},
	})...)
	if err := n.do(unix.RTM_NEWLINK, unix.NLM_F_CREATE|unix.NLM_F_EXCL, body); err != nil {
		return fmt.Errorf("guestnet: creating the bridge %s: %w", name, err)
	}
	return nil
}

func (n *netlink) setLink(index, master int, up bool) error {
	var flags, change uint32
	if up {
		flags, change = unix.IFF_UP, unix.IFF_UP
	}
	body := ifinfo(index, flags, change)
	if master > 0 {
		body = append(body, encodeAttrs([]attr{{unix.IFLA_MASTER, u32(uint32(master))}})...)
	}
	return n.do(unix.RTM_NEWLINK, 0, body)
}

func (n *netlink) permanentNeighbor(index int, address, mac string) error {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return err
	}
	hw, err := ParseMAC(mac)
	if err != nil {
		return err
	}
	nd := make([]byte, unix.SizeofNdMsg)
	*(*unix.NdMsg)(unsafe.Pointer(&nd[0])) = unix.NdMsg{Family: unix.AF_INET, Ifindex: int32(index), State: unix.NUD_PERMANENT}
	v4 := ip.As4()
	body := append(nd, encodeAttrs([]attr{{unix.NDA_DST, v4[:]}, {unix.NDA_LLADDR, hw[:]}})...)
	return n.do(unix.RTM_NEWNEIGH, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body)
}

func (n *netlink) hostRoute(index int, address string) error {
	ip, err := netip.ParseAddr(address)
	if err != nil {
		return err
	}
	rt := make([]byte, unix.SizeofRtMsg)
	*(*unix.RtMsg)(unsafe.Pointer(&rt[0])) = unix.RtMsg{
		Family: unix.AF_INET, Dst_len: 32, Table: unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_BOOT, Scope: unix.RT_SCOPE_LINK, Type: unix.RTN_UNICAST,
	}
	v4 := ip.As4()
	body := append(rt, encodeAttrs([]attr{{unix.RTA_DST, v4[:]}, {unix.RTA_OIF, u32(uint32(index))}})...)
	return n.do(unix.RTM_NEWROUTE, unix.NLM_F_CREATE|unix.NLM_F_REPLACE, body)
}

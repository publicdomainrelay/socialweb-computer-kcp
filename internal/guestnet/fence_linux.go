//go:build linux

package guestnet

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	FenceTable = "swcguest"

	fencePriority int32 = -300

	fenceAnswerWait = 5 * time.Second

	brPreRouting = 0
	brLocalIn    = 1

	verdictDrop   = 0
	verdictAccept = 1

	ethIPv4 = 0x0800
	ethIPv6 = 0x86dd
	ethARP  = 0x0806

	ipTCP    = 6
	ipUDP    = 17
	ipICMPv6 = 58

	icmpv6RouterSolicit   = 133
	icmpv6NeighborSolicit = 135
	icmpv6NeighborAdvert  = 136
)

type expr struct {
	name  string
	attrs []attr
}

type fenceRule struct {
	chain string
	exprs []expr
}

type nftMessage struct {
	typ   uint16
	flags uint16
	body  []byte
}

type ipFamily struct {
	match  []expr
	dstOff uint32
}

func (f ipFamily) to(p netip.Prefix) []expr {
	return slices.Concat(f.match, prefixIs(unix.NFT_PAYLOAD_NETWORK_HEADER, f.dstOff, p))
}

func Fence(w Wiring) error {
	host, err := netip.ParseAddr(HostAddress)
	if err != nil || !host.Is4() {
		return fmt.Errorf("guestnet: host address %q is not IPv4", HostAddress)
	}
	gateway, err := netip.ParseAddr(Gateway)
	if err != nil || !gateway.Is4() {
		return fmt.Errorf("guestnet: gateway %q is not IPv4", Gateway)
	}
	ports := HostPorts(w.HostPorts)
	if len(ports) != len(w.HostPorts) {
		return fmt.Errorf("guestnet: host ports %v carry a duplicate or a value outside 1-65535", w.HostPorts)
	}
	if len(w.HostAddresses) == 0 {
		return fmt.Errorf("guestnet: the wiring names none of the host's addresses")
	}
	own, err := ParsePrefixes(w.HostAddresses)
	if err != nil {
		return err
	}
	link, err := ParsePrefixes([]string{GuestLink})
	if err != nil {
		return err
	}
	blocked, err := ParsePrefixes(BlockedPrefixes)
	if err != nil {
		return err
	}
	lan, err := ParsePrefixes(LANUDPPrefixes)
	if err != nil {
		return err
	}
	fromGuest := metaIs(unix.NFT_META_IIFNAME, ifname(w.Tap))
	v4 := ipFamily{slices.Concat(fromGuest, metaIs(unix.NFT_META_PROTOCOL, be16(ethIPv4))), 16}
	v6 := ipFamily{slices.Concat(fromGuest, metaIs(unix.NFT_META_PROTOCOL, be16(ethIPv6))), 24}
	familyOf := func(p netip.Prefix) ipFamily {
		if p.Addr().Is4() {
			return v4
		}
		return v6
	}
	toHost := v4.to(netip.PrefixFrom(host, 32))
	var rules []fenceRule
	pre := func(parts ...[]expr) {
		rules = append(rules, fenceRule{"prerouting", slices.Concat(parts...)})
	}
	pre(fromGuest, metaLoad(unix.NFT_META_PROTOCOL),
		[]expr{cmpOp(unix.NFT_CMP_NEQ, be16(ethIPv4)), cmpOp(unix.NFT_CMP_NEQ, be16(ethIPv6)), cmpOp(unix.NFT_CMP_NEQ, be16(ethARP))},
		verdict(verdictDrop))
	for _, p := range ports {
		pre(toHost, l4Is(ipTCP), dport(cmpOp(unix.NFT_CMP_EQ, be16(uint16(p)))), verdict(verdictAccept))
	}
	pre(toHost, l4Is(ipTCP), rejectTCP())
	pre(toHost, verdict(verdictDrop))
	for _, proto := range []byte{ipUDP, ipTCP} {
		pre(v4.to(netip.PrefixFrom(gateway, 32)), l4Is(proto), dport(cmpOp(unix.NFT_CMP_EQ, be16(DNSPort))), verdict(verdictAccept))
	}
	for _, t := range []byte{icmpv6RouterSolicit, icmpv6NeighborSolicit, icmpv6NeighborAdvert} {
		pre(v6.match, l4Is(ipICMPv6), payloadLoad(unix.NFT_PAYLOAD_TRANSPORT_HEADER, 0, 1), []expr{cmpOp(unix.NFT_CMP_EQ, []byte{t})},
			verdict(verdictAccept))
	}
	highUDP := []expr{cmpOp(unix.NFT_CMP_GTE, be16(HighUDPMinPort))}
	for _, closed := range HighUDPClosedPorts {
		highUDP = append(highUDP, cmpOp(unix.NFT_CMP_NEQ, be16(uint16(closed))))
	}
	if w.HostUDP {
		for _, p := range HostUDPPrefixes(own) {
			pre(familyOf(p).to(p), l4Is(ipUDP), dport(highUDP...), verdict(verdictAccept))
		}
	}
	for _, p := range slices.Concat(own, link) {
		pre(familyOf(p).to(p), verdict(verdictDrop))
	}
	if w.LANUDP {
		for _, p := range lan {
			pre(familyOf(p).to(p), l4Is(ipUDP), dport(highUDP...), verdict(verdictAccept))
		}
	}
	for _, p := range blocked {
		pre(familyOf(p).to(p), verdict(verdictDrop))
	}
	rules = append(rules, fenceRule{"input", slices.Concat(fromGuest, verdict(verdictDrop))})
	msgs := []nftMessage{{typ: unix.NFT_MSG_NEWTABLE, flags: unix.NLM_F_CREATE | unix.NLM_F_EXCL,
		body: encodeAttrs([]attr{{unix.NFTA_TABLE_NAME, cstr(FenceTable)}})}}
	for _, c := range []struct {
		name string
		hook uint32
	}{{"prerouting", brPreRouting}, {"input", brLocalIn}} {
		hook := encodeAttrs([]attr{{unix.NFTA_HOOK_HOOKNUM, be32(c.hook)}, {unix.NFTA_HOOK_PRIORITY, beInt32(fencePriority)}})
		msgs = append(msgs, nftMessage{typ: unix.NFT_MSG_NEWCHAIN, flags: unix.NLM_F_CREATE | unix.NLM_F_EXCL, body: encodeAttrs([]attr{
			{unix.NFTA_CHAIN_TABLE, cstr(FenceTable)},
			{unix.NFTA_CHAIN_NAME, cstr(c.name)},
			{unix.NFTA_CHAIN_HOOK | unix.NLA_F_NESTED, hook},
			{unix.NFTA_CHAIN_POLICY, be32(verdictAccept)},
			{unix.NFTA_CHAIN_TYPE, cstr("filter")},
		})})
	}
	for _, r := range rules {
		list := make([]attr, 0, len(r.exprs))
		for _, e := range r.exprs {
			list = append(list, attr{unix.NFTA_LIST_ELEM | unix.NLA_F_NESTED, encodeAttrs([]attr{
				{unix.NFTA_EXPR_NAME, cstr(e.name)},
				{unix.NFTA_EXPR_DATA | unix.NLA_F_NESTED, encodeAttrs(e.attrs)},
			})})
		}
		msgs = append(msgs, nftMessage{typ: unix.NFT_MSG_NEWRULE, flags: unix.NLM_F_CREATE | unix.NLM_F_APPEND, body: encodeAttrs([]attr{
			{unix.NFTA_RULE_TABLE, cstr(FenceTable)},
			{unix.NFTA_RULE_CHAIN, cstr(r.chain)},
			{unix.NFTA_RULE_EXPRESSIONS | unix.NLA_F_NESTED, encodeAttrs(list)},
		})})
	}
	if err := nftBatch(msgs); err != nil {
		return fmt.Errorf("guestnet: fencing the guest off the host and the LAN with nf_tables: %w", err)
	}
	return nil
}

func metaLoad(key uint32) []expr {
	return []expr{{"meta", []attr{{unix.NFTA_META_DREG, be32(unix.NFT_REG_1)}, {unix.NFTA_META_KEY, be32(key)}}}}
}

func metaIs(key uint32, value []byte) []expr {
	return append(metaLoad(key), cmpOp(unix.NFT_CMP_EQ, value))
}

func l4Is(proto byte) []expr {
	return metaIs(unix.NFT_META_L4PROTO, []byte{proto})
}

func payloadLoad(base, offset, length uint32) []expr {
	return []expr{{"payload", []attr{
		{unix.NFTA_PAYLOAD_DREG, be32(unix.NFT_REG_1)},
		{unix.NFTA_PAYLOAD_BASE, be32(base)},
		{unix.NFTA_PAYLOAD_OFFSET, be32(offset)},
		{unix.NFTA_PAYLOAD_LEN, be32(length)},
	}}}
}

func prefixIs(base, offset uint32, p netip.Prefix) []expr {
	addr := p.Masked().Addr().AsSlice()
	load := payloadLoad(base, offset, uint32(len(addr)))
	if p.Bits() == len(addr)*8 {
		return append(load, cmpOp(unix.NFT_CMP_EQ, addr))
	}
	mask := make([]byte, len(addr))
	for i := 0; i < p.Bits(); i++ {
		mask[i/8] |= 0x80 >> (i % 8)
	}
	return append(load, bitwise(mask), cmpOp(unix.NFT_CMP_EQ, addr))
}

func dport(cmps ...expr) []expr {
	return append(payloadLoad(unix.NFT_PAYLOAD_TRANSPORT_HEADER, 2, 2), cmps...)
}

func bitwise(mask []byte) expr {
	return expr{"bitwise", []attr{
		{unix.NFTA_BITWISE_SREG, be32(unix.NFT_REG_1)},
		{unix.NFTA_BITWISE_DREG, be32(unix.NFT_REG_1)},
		{unix.NFTA_BITWISE_LEN, be32(uint32(len(mask)))},
		{unix.NFTA_BITWISE_MASK | unix.NLA_F_NESTED, encodeAttrs([]attr{{unix.NFTA_DATA_VALUE, mask}})},
		{unix.NFTA_BITWISE_XOR | unix.NLA_F_NESTED, encodeAttrs([]attr{{unix.NFTA_DATA_VALUE, make([]byte, len(mask))}})},
	}}
}

func cmpOp(op uint32, value []byte) expr {
	return expr{"cmp", []attr{
		{unix.NFTA_CMP_SREG, be32(unix.NFT_REG_1)},
		{unix.NFTA_CMP_OP, be32(op)},
		{unix.NFTA_CMP_DATA | unix.NLA_F_NESTED, encodeAttrs([]attr{{unix.NFTA_DATA_VALUE, value}})},
	}}
}

func verdict(code uint32) []expr {
	v := encodeAttrs([]attr{{unix.NFTA_VERDICT_CODE, be32(code)}})
	return []expr{{"immediate", []attr{
		{unix.NFTA_IMMEDIATE_DREG, be32(unix.NFT_REG_VERDICT)},
		{unix.NFTA_IMMEDIATE_DATA | unix.NLA_F_NESTED, encodeAttrs([]attr{{unix.NFTA_DATA_VERDICT | unix.NLA_F_NESTED, v}})},
	}}}
}

func rejectTCP() []expr {
	return []expr{{"reject", []attr{{unix.NFTA_REJECT_TYPE, be32(unix.NFT_REJECT_TCP_RST)}, {unix.NFTA_REJECT_ICMP_CODE, []byte{0}}}}}
}

func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

func be16(v uint16) []byte {
	return binary.BigEndian.AppendUint16(nil, v)
}

func be32(v uint32) []byte {
	return binary.BigEndian.AppendUint32(nil, v)
}

func beInt32(v int32) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(v))
}

func nftBatch(msgs []nftMessage) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_NETFILTER)
	if err != nil {
		return fmt.Errorf("netfilter netlink socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netfilter netlink bind: %w", err)
	}
	tv := unix.NsecToTimeval(fenceAnswerWait.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	var buf []byte
	seq := uint32(1)
	frame := func(typ, flags uint16, family uint8, resID uint16, body []byte) {
		l := unix.SizeofNlMsghdr + 4 + len(body)
		msg := make([]byte, unix.SizeofNlMsghdr, align4(l))
		*(*unix.NlMsghdr)(unsafe.Pointer(&msg[0])) = unix.NlMsghdr{Len: uint32(l), Type: typ, Flags: unix.NLM_F_REQUEST | flags, Seq: seq}
		msg = append(msg, family, unix.NFNETLINK_V0, byte(resID>>8), byte(resID))
		msg = append(msg, body...)
		buf = append(buf, msg[:align4(l)]...)
		seq++
	}
	frame(unix.NFNL_MSG_BATCH_BEGIN, 0, unix.AF_UNSPEC, unix.NFNL_SUBSYS_NFTABLES, nil)
	first := seq
	for _, m := range msgs {
		frame(unix.NFNL_SUBSYS_NFTABLES<<8|m.typ, m.flags|unix.NLM_F_ACK, unix.NFPROTO_BRIDGE, 0, m.body)
	}
	end := seq
	frame(unix.NFNL_MSG_BATCH_END, 0, unix.AF_UNSPEC, unix.NFNL_SUBSYS_NFTABLES, nil)
	if err := unix.Sendto(fd, buf, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	pending := map[uint32]bool{}
	for s := first; s < end; s++ {
		pending[s] = true
	}
	rx := make([]byte, 4*os.Getpagesize())
	for len(pending) > 0 {
		n, _, err := unix.Recvfrom(fd, rx, 0)
		if err != nil {
			return fmt.Errorf("waiting for the kernel's answer: %w", err)
		}
		parsed, err := syscall.ParseNetlinkMessage(rx[:n])
		if err != nil {
			return err
		}
		for _, m := range parsed {
			if m.Header.Type != unix.NLMSG_ERROR || !pending[m.Header.Seq] {
				continue
			}
			if len(m.Data) < 4 {
				return fmt.Errorf("short netlink error message")
			}
			if code := int32(binary.NativeEndian.Uint32(m.Data[:4])); code != 0 {
				return fmt.Errorf("message %d of %d: %w", m.Header.Seq-first+1, end-first, unix.Errno(-code))
			}
			delete(pending, m.Header.Seq)
		}
	}
	return nil
}

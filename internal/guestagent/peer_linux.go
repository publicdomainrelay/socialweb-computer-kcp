//go:build linux

package guestagent

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
)

func loopbackPeerIsRoot(c net.Conn) bool {
	local, lok := c.LocalAddr().(*net.TCPAddr)
	remote, rok := c.RemoteAddr().(*net.TCPAddr)
	if !lok || !rok {
		return false
	}
	f, err := os.Open("/proc/net/tcp")
	if err != nil {
		return false
	}
	defer f.Close()
	peer, self := procAddr(remote), procAddr(local)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 7 && fields[1] == peer && fields[2] == self {
			return fields[7] == "0"
		}
	}
	return false
}

func procAddr(a *net.TCPAddr) string {
	ip := a.IP.To4()
	if ip == nil {
		return ""
	}
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", ip[3], ip[2], ip[1], ip[0], a.Port)
}

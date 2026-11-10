//go:build !linux

package guestagent

import "net"

func loopbackPeerIsRoot(net.Conn) bool { return true }

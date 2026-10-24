//go:build linux

package apiserverfront

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func ListenerFromFD(fd int) (net.Listener, error) {
	unix.CloseOnExec(fd)
	f := os.NewFile(uintptr(fd), FDName)
	if f == nil {
		return nil, fmt.Errorf("apiserverfront: fd %d is not open", fd)
	}
	defer f.Close()
	ln, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("apiserverfront: the inherited fd %d is no listening socket: %w", fd, err)
	}
	if _, ok := ln.(*net.TCPListener); !ok {
		ln.Close()
		return nil, fmt.Errorf("apiserverfront: the inherited fd %d is not a TCP listener", fd)
	}
	return ln, nil
}

func HeldFromFD(fd int, name string) (*Held, error) {
	unix.CloseOnExec(fd)
	port, err := boundPort(fd)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("apiserverfront: the inherited hold %q (fd %d): %w", name, fd, err)
	}
	return &Held{Name: name, Port: port, f: os.NewFile(uintptr(fd), HoldFDPrefix+name)}, nil
}

func boundPort(fd int) (int, error) {
	sa, err := unix.Getsockname(fd)
	if err != nil {
		return 0, err
	}
	switch a := sa.(type) {
	case *unix.SockaddrInet6:
		return a.Port, nil
	case *unix.SockaddrInet4:
		return a.Port, nil
	}
	return 0, errors.New("not an inet socket")
}

func bindHold(name string, port int) (*Held, error) {
	h, err := bindHoldOnce(name, port, false)
	if err == nil || port == 0 || !errors.Is(err, unix.EADDRINUSE) || !onlyTimeWait(port) {
		return h, err
	}
	return bindHoldOnce(name, port, true)
}

func onlyTimeWait(port int) bool {
	waiting, other := 0, 0
	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(table)
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for _, line := range lines[min(1, len(lines)):] {
			f := strings.Fields(line)
			if len(f) < 4 {
				continue
			}
			i := strings.LastIndexByte(f[1], ':')
			if i < 0 {
				continue
			}
			local, err := strconv.ParseUint(f[1][i+1:], 16, 16)
			if err != nil || int(local) != port {
				continue
			}
			if f[3] == tcpTimeWait {
				waiting++
			} else {
				other++
			}
		}
	}
	return waiting > 0 && other == 0
}

const tcpTimeWait = "06"

func bindHoldOnce(name string, port int, reuseFirst bool) (*Held, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	var sa unix.Sockaddr = &unix.SockaddrInet6{Port: port}
	if err == nil {
		if err = unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, 0); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("apiserverfront: hold %q: a dual-stack socket: %w", name, err)
		}
	} else {
		if fd, err = unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0); err != nil {
			return nil, fmt.Errorf("apiserverfront: hold %q: %w", name, err)
		}
		sa = &unix.SockaddrInet4{Port: port}
	}
	if reuseFirst {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("apiserverfront: hold %q: SO_REUSEADDR before bind over TIME_WAIT: %w", name, err)
		}
	}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("apiserverfront: hold %q on port %d: %w", name, port, err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("apiserverfront: hold %q: SO_REUSEADDR after bind: %w", name, err)
	}
	got, err := boundPort(fd)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("apiserverfront: hold %q: %w", name, err)
	}
	return &Held{Name: name, Port: got, f: os.NewFile(uintptr(fd), HoldFDPrefix+name)}, nil
}

func storeListener(sock string, ln net.Listener) error {
	tl, ok := ln.(*net.TCPListener)
	if !ok {
		return errors.New("apiserverfront: only a TCP listener goes into the fd store")
	}
	raw, err := tl.SyscallConn()
	if err != nil {
		return err
	}
	var werr error
	err = raw.Control(func(fd uintptr) { werr = storeFD(sock, int(fd), "FDNAME="+FDName) })
	if err != nil {
		return err
	}
	return werr
}

func storeHold(sock string, h *Held) error {
	raw, err := h.f.SyscallConn()
	if err != nil {
		return err
	}
	var werr error
	err = raw.Control(func(fd uintptr) { werr = storeFD(sock, int(fd), "FDNAME="+HoldFDPrefix+h.Name+"\nFDPOLL=0") })
	if err != nil {
		return err
	}
	return werr
}

func storeFD(sock string, fd int, fields string) error {
	s, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("apiserverfront: a socket for the fd store at %s: %w", sock, err)
	}
	defer unix.Close(s)
	if err := unix.Sendmsg(s, []byte("FDSTORE=1\n"+fields), unix.UnixRights(fd), &unix.SockaddrUnix{Name: sock}, 0); err != nil {
		return fmt.Errorf("apiserverfront: handing %q to the fd store at %s: %w", fields, sock, err)
	}
	return nil
}

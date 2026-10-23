package apiserverfront

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	EnvNotifySocket = "NOTIFY_SOCKET"
	EnvListenFDs    = "LISTEN_FDS"
	EnvListenPID    = "LISTEN_PID"
	EnvListenNames  = "LISTEN_FDNAMES"
	FDName          = "front"
	HoldFDPrefix    = "hold."
	listenFDStart   = 3
)

type Acquired struct {
	Listener  net.Listener
	Inherited bool
	Holds     map[string]*Held
	Notify    string
}

func inherited(getenv func(string) string) (int, []string, error) {
	v := getenv(EnvListenFDs)
	if v == "" {
		return 0, nil, nil
	}
	if pid := getenv(EnvListenPID); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0, nil, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, nil, fmt.Errorf("apiserverfront: %s=%q is no count", EnvListenFDs, v)
	}
	var names []string
	if raw := getenv(EnvListenNames); raw != "" {
		names = strings.Split(raw, ":")
	}
	if len(names) != n {
		if n > 1 {
			return 0, nil, fmt.Errorf("apiserverfront: %s=%d with %s=%q: a front with held ports needs every fd named",
				EnvListenFDs, n, EnvListenNames, getenv(EnvListenNames))
		}
		names = make([]string, n)
		for i := range names {
			names[i] = FDName
		}
	}
	return n, names, nil
}

func Acquire(address string, fdStore bool, getenv func(string) string) (net.Listener, bool, error) {
	a, err := AcquireAll(address, fdStore, getenv)
	if err != nil {
		return nil, false, err
	}
	for _, h := range a.Holds {
		h.Close()
	}
	return a.Listener, a.Inherited, nil
}

func AcquireAll(address string, fdStore bool, getenv func(string) string) (Acquired, error) {
	n, names, err := inherited(getenv)
	if err != nil {
		return Acquired{}, err
	}
	notify := getenv(EnvNotifySocket)
	for _, k := range []string{EnvListenFDs, EnvListenPID, EnvListenNames} {
		os.Unsetenv(k)
	}
	out := Acquired{Holds: map[string]*Held{}, Notify: notify}
	fail := func(err error) (Acquired, error) {
		if out.Listener != nil {
			out.Listener.Close()
		}
		for _, h := range out.Holds {
			h.Close()
		}
		return Acquired{}, err
	}
	for i := range n {
		fd := listenFDStart + i
		switch name := names[i]; {
		case name == FDName:
			if out.Listener != nil {
				return fail(fmt.Errorf("apiserverfront: two inherited fds are named %q; the front holds exactly one listening socket", FDName))
			}
			ln, err := ListenerFromFD(fd)
			if err != nil {
				return fail(err)
			}
			out.Listener, out.Inherited = ln, true
		case strings.HasPrefix(name, HoldFDPrefix):
			h, err := HeldFromFD(fd, strings.TrimPrefix(name, HoldFDPrefix))
			if err != nil {
				return fail(err)
			}
			out.Holds[h.Name] = h
		default:
			return fail(fmt.Errorf("apiserverfront: inherited fd %d is named %q, which is neither %q nor %s<name>", fd, name, FDName, HoldFDPrefix))
		}
	}
	if out.Listener != nil {
		return out, nil
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return fail(fmt.Errorf("apiserverfront: listening on %s: %w", address, err))
	}
	out.Listener = ln
	if fdStore {
		if notify == "" {
			return fail(fmt.Errorf("apiserverfront: an fd-store front needs %s from its service manager", EnvNotifySocket))
		}
		if err := storeListener(notify, ln); err != nil {
			return fail(err)
		}
	}
	return out, nil
}

//go:build darwin

package hostsvc

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ponytail: lsof per call, about 25 ms; swap for proc_pidinfo(PROC_PIDLISTFDS) when the poll cost shows.
const lsofPath = "/usr/sbin/lsof"

func ListenPort(pid int) (int, error) {
	all, err := ListenPorts(pid)
	if err != nil {
		return 0, err
	}
	switch len(all) {
	case 0:
		return 0, fmt.Errorf("%w: pid %d", ErrNoListener, pid)
	case 1:
		return all[0], nil
	}
	return 0, fmt.Errorf("%w: pid %d listens on %v; the single-socket rule refuses to guess", ErrManyListeners, pid, all)
}

func ListenPorts(pid int) ([]int, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("%w: pid %d", ErrNoListener, pid)
	}
	out, err := exec.Command(lsofPath, "-nP", "-a", "-p", strconv.Itoa(pid), "-iTCP", "-sTCP:LISTEN", "-Fn").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hostsvc: reading the listening sockets of pid %d: %w", pid, err)
	}
	seen := map[int]bool{}
	var ports []int
	for line := range strings.Lines(string(out)) {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "n")
		if !ok || name == "" {
			continue
		}
		i := strings.LastIndexByte(name, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(name[i+1:])
		if err != nil {
			return nil, fmt.Errorf("hostsvc: lsof names the address %q: %w", name, err)
		}
		if !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	return ports, nil
}

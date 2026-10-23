package ports

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	PortsFile = "ports.json"

	DefaultPortFirst = 20000

	DefaultPortLast = 29999

	DefaultPortHost = "127.0.0.1"
)

type Lease struct {
	Name string `json:"name"`

	Host string `json:"host"`

	Port int `json:"port"`

	AllocatedAt time.Time `json:"allocatedAt"`
}

func (l Lease) Address() string {
	return net.JoinHostPort(l.Host, strconv.Itoa(l.Port))
}

type Options struct {
	StateDir string

	First int

	Last int

	Host string
}

type portFile struct {
	Version int `json:"version"`

	Ports []Lease `json:"ports"`
}

const portFileVersion = 1

type Registry struct {
	path string

	host string

	first int

	last int

	now func() time.Time

	mu sync.Mutex

	leases map[string]Lease
}

func New(stateDir string) (*Registry, error) {
	return Open(Options{StateDir: stateDir})
}

func Open(opts Options) (*Registry, error) {
	stateDir := strings.TrimSpace(opts.StateDir)
	if stateDir == "" {
		return nil, errors.New("ports: the port registry needs a state dir")
	}
	if opts.First == 0 && opts.Last == 0 {
		opts.First, opts.Last = DefaultPortFirst, DefaultPortLast
	}
	switch {
	case opts.First < 1 || opts.First > 65535:
		return nil, fmt.Errorf("ports: the port registry first port %d is out of range", opts.First)
	case opts.Last < 1 || opts.Last > 65535:
		return nil, fmt.Errorf("ports: the port registry last port %d is out of range", opts.Last)
	case opts.Last < opts.First:
		return nil, fmt.Errorf("ports: the port registry range %d-%d is empty", opts.First, opts.Last)
	}
	host := strings.TrimSpace(opts.Host)
	host = cmp.Or(host, DefaultPortHost)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("ports: creating the port registry state dir: %w", err)
	}
	r := &Registry{
		path:   filepath.Join(stateDir, PortsFile),
		host:   host,
		first:  opts.First,
		last:   opts.Last,
		now:    time.Now,
		leases: map[string]Lease{},
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

func (r *Registry) Allocate(name string) (Lease, error) {
	if r == nil {
		return Lease{}, errors.New("ports: the port registry is nil")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Lease{}, errors.New("ports: a port lease needs a name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if lease, ok := r.leases[name]; ok {
		return lease, nil
	}
	used := make(map[int]bool, len(r.leases))
	for _, lease := range r.leases {
		used[lease.Port] = true
	}
	port := 0
	for candidate := r.first; candidate <= r.last; candidate++ {
		if used[candidate] {
			continue
		}
		probe, err := net.Listen("tcp", net.JoinHostPort(r.host, strconv.Itoa(candidate)))
		if err != nil {
			continue
		}
		_ = probe.Close()
		port = candidate
		break
	}
	if port == 0 {
		return Lease{}, fmt.Errorf("ports: the port registry range %d-%d is full", r.first, r.last)
	}
	lease := Lease{Name: name, Host: r.host, Port: port, AllocatedAt: r.now().UTC()}
	r.leases[name] = lease
	if err := r.save(); err != nil {
		delete(r.leases, name)
		return Lease{}, err
	}
	return lease, nil
}

func (r *Registry) Listen(name string) (net.Listener, Lease, error) {
	lease, err := r.Allocate(name)
	if err != nil {
		return nil, Lease{}, err
	}
	listener, err := net.Listen("tcp", lease.Address())
	if err != nil {
		return nil, lease, fmt.Errorf("ports: listening on the port kept for %s at %s: %w", name, lease.Address(), err)
	}
	return listener, lease, nil
}

func (r *Registry) Lease(name string) (Lease, bool) {
	if r == nil {
		return Lease{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, ok := r.leases[strings.TrimSpace(name)]
	return lease, ok
}

func (r *Registry) Address(name string) (string, bool) {
	lease, ok := r.Lease(name)
	if !ok {
		return "", false
	}
	return lease.Address(), true
}

func (r *Registry) Release(name string) error {
	if r == nil {
		return errors.New("ports: the port registry is nil")
	}
	name = strings.TrimSpace(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.leases[name]; !ok {
		return nil
	}
	delete(r.leases, name)
	return r.save()
}

func (r *Registry) Leases() []Lease {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Lease, 0, len(r.leases))
	for _, lease := range r.leases {
		out = append(out, lease)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("ports: reading the port registry %s: %w", r.path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var file portFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("ports: the port registry %s is not JSON: %w", r.path, err)
	}
	if file.Version != 0 && file.Version != portFileVersion {
		return fmt.Errorf("ports: the port registry %s has version %d, not %d", r.path, file.Version, portFileVersion)
	}
	names := make(map[string]bool, len(file.Ports))
	ports := make(map[int]string, len(file.Ports))
	for _, lease := range file.Ports {
		lease.Name = strings.TrimSpace(lease.Name)
		switch {
		case lease.Name == "":
			return fmt.Errorf("ports: the port registry %s holds a lease with no name", r.path)
		case names[lease.Name]:
			return fmt.Errorf("ports: the port registry %s names %s twice", r.path, lease.Name)
		case lease.Port < 1 || lease.Port > 65535:
			return fmt.Errorf("ports: the port registry %s gives %s the port %d", r.path, lease.Name, lease.Port)
		case ports[lease.Port] != "":
			return fmt.Errorf("ports: the port registry %s gives the port %d to %s and %s", r.path, lease.Port, ports[lease.Port], lease.Name)
		}
		lease.Host = cmp.Or(lease.Host, r.host)
		names[lease.Name] = true
		ports[lease.Port] = lease.Name
		r.leases[lease.Name] = lease
	}
	return nil
}

func (r *Registry) save() error {
	file := portFile{Version: portFileVersion, Ports: make([]Lease, 0, len(r.leases))}
	for _, lease := range r.leases {
		file.Ports = append(file.Ports, lease)
	}
	sort.Slice(file.Ports, func(i, j int) bool { return file.Ports[i].Name < file.Ports[j].Name })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("ports: encoding the port registry: %w", err)
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, ".ports-*")
	if err != nil {
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	if err := os.Rename(name, r.path); err != nil {
		return fmt.Errorf("ports: writing the port registry: %w", err)
	}
	return nil
}

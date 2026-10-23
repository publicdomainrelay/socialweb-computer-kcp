package portallocator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
)

const (
	DefaultFirst = 20000
	DefaultLast  = 29999

	DevFirst = 15000
	DevLast  = 19999
)

type Range struct {
	First int
	Last  int
}

func (r Range) Contains(p int) bool { return p >= r.First && p <= r.Last }

func (r Range) Overlaps(o Range) bool { return r.First <= o.Last && o.First <= r.Last }

func (r Range) Empty() bool { return r == Range{} || r.Last < r.First }

func (r Range) String() string { return fmt.Sprintf("%d-%d", r.First, r.Last) }

type Purpose string

const (
	PurposePostgres Purpose = "postgres"

	PurposeOpenBao Purpose = "openbao"

	PurposeKine Purpose = "kine"

	PurposeK3sSecure Purpose = "k3s-secure"

	PurposeK3sControllerManager      Purpose = "k3s-controller-manager"
	PurposeK3sScheduler              Purpose = "k3s-scheduler"
	PurposeK3sCloudControllerManager Purpose = "k3s-cloud-controller-manager"

	PurposeK3sSlotA Purpose = "k3s-slot-a"
	PurposeK3sSlotB Purpose = "k3s-slot-b"

	PurposeK3sApiserverA Purpose = "k3s-apiserver-a"
	PurposeK3sApiserverB Purpose = "k3s-apiserver-b"

	PurposeKineB                      Purpose = "kine-b"
	PurposeK3sControllerManagerB      Purpose = "k3s-controller-manager-b"
	PurposeK3sSchedulerB              Purpose = "k3s-scheduler-b"
	PurposeK3sCloudControllerManagerB Purpose = "k3s-cloud-controller-manager-b"

	PurposeExternalSecretsHealth Purpose = "external-secrets-health"

	PurposeExternalSecretsWebhook Purpose = "external-secrets-webhook"

	PurposeExternalSecretsWebhookHealth Purpose = "external-secrets-webhook-health"
)

func (p Purpose) Datagram() bool { return false }

var retiredPurposes = map[Purpose]bool{
	"ssh-server":       true,
	"ssh-node":         true,
	"advertise":        true,
	"kubelet":          true,
	"apiserver-link":   true,
	"openbao-link":     true,
	"apiserver-link-b": true,
	"openbao-link-b":   true,
}

var knownPurposes = map[Purpose]bool{
	PurposePostgres:                     true,
	PurposeOpenBao:                      true,
	PurposeKine:                         true,
	PurposeK3sSecure:                    true,
	PurposeK3sControllerManager:         true,
	PurposeK3sScheduler:                 true,
	PurposeK3sCloudControllerManager:    true,
	PurposeK3sSlotA:                     true,
	PurposeK3sSlotB:                     true,
	PurposeK3sApiserverA:                true,
	PurposeK3sApiserverB:                true,
	PurposeKineB:                        true,
	PurposeK3sControllerManagerB:        true,
	PurposeK3sSchedulerB:                true,
	PurposeK3sCloudControllerManagerB:   true,
	PurposeExternalSecretsHealth:        true,
	PurposeExternalSecretsWebhook:       true,
	PurposeExternalSecretsWebhookHealth: true,
}

func (p Purpose) Width() int {
	if knownPurposes[p] {
		return 1
	}
	return 0
}

type SlotPurposes struct {
	Secure                 Purpose
	Apiserver              Purpose
	Kine                   Purpose
	ControllerManager      Purpose
	Scheduler              Purpose
	CloudControllerManager Purpose
}

func (s SlotPurposes) All() []Purpose {
	return []Purpose{s.Secure, s.Apiserver, s.Kine, s.ControllerManager, s.Scheduler, s.CloudControllerManager}
}

var slotPurposes = map[string]SlotPurposes{
	"a": {PurposeK3sSlotA, PurposeK3sApiserverA, PurposeKine, PurposeK3sControllerManager, PurposeK3sScheduler, PurposeK3sCloudControllerManager},
	"b": {PurposeK3sSlotB, PurposeK3sApiserverB, PurposeKineB, PurposeK3sControllerManagerB, PurposeK3sSchedulerB, PurposeK3sCloudControllerManagerB},
}

var ControlPlaneSlots = []string{"a", "b"}

func PurposesOfSlot(slot string) (SlotPurposes, error) {
	s, ok := slotPurposes[slot]
	if !ok {
		return SlotPurposes{}, fmt.Errorf("portallocator: %q is no control-plane slot; the slots are %v", slot, ControlPlaneSlots)
	}
	return s, nil
}

type Lease struct {
	Port    int     `json:"port"`
	Width   int     `json:"width"`
	Purpose Purpose `json:"purpose"`
	Cluster string  `json:"cluster"`
	Account string  `json:"account,omitempty"`

	Instance string `json:"instance,omitempty"`

	AppInstance string `json:"appInstance,omitempty"`

	AllocatedAt time.Time `json:"allocatedAt"`
}

func (l Lease) Covers(p int) bool { return p >= l.Port && p < l.Port+l.Width }

const leaseFileVersion = 1

type leaseFile struct {
	Version int     `json:"version"`
	Leases  []Lease `json:"leases"`
}

type Holder func(ctx context.Context, cluster string, p Purpose) (int, error)

var ErrNoHolder = errors.New("portallocator: no holder: a port is chosen by the kernel and held by the " +
	"cluster's apiserver front, and this allocator was opened without a way to reach one")

type Config struct {
	LeasePath string

	Holder Holder

	ReclaimGrace time.Duration

	Now func() time.Time
}

type Allocator struct {
	app    string
	path   string
	holder Holder
	grace  time.Duration
	now    func() time.Time

	mu     sync.Mutex
	leases map[leaseKey]Lease
}

type leaseKey struct {
	app     string
	cluster string
	purpose Purpose
}

func Open(cfg Config) (*Allocator, error) {
	if cfg.LeasePath == "" {
		return nil, errors.New("portallocator: LeasePath is required; the lease file records which port the front holds for which purpose")
	}
	a := &Allocator{
		path:   cfg.LeasePath,
		holder: cfg.Holder,
		grace:  cfg.ReclaimGrace,
		now:    cfg.Now,
		leases: map[leaseKey]Lease{},
	}
	if a.now == nil {
		a.now = time.Now
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Allocator) SetHolder(h Holder) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.holder = h
}

func (a *Allocator) lockPath() string { return a.path + ".lock" }

func (a *Allocator) withLock(how int, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("portallocator: lease dir: %w", err)
	}
	f, err := os.OpenFile(a.lockPath(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("portallocator: lease lock %s: %w", a.lockPath(), err)
	}
	defer f.Close()
	if err := hostos.Lock(f, how); err != nil {
		return fmt.Errorf("portallocator: locking %s: %w", a.lockPath(), err)
	}
	defer func() { _ = hostos.Unlock(f) }()
	return fn()
}

func (a *Allocator) load() error {
	leases, err := a.readFile()
	if err != nil {
		return err
	}
	a.leases = leases
	return nil
}

func (a *Allocator) readFile() (map[leaseKey]Lease, error) {
	data, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[leaseKey]Lease{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("portallocator: reading lease file %s: %w", a.path, err)
	}
	var f leaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("portallocator: lease file %s is unreadable, refusing to start with an empty store over it: %w", a.path, err)
	}
	if f.Version != leaseFileVersion {
		return nil, fmt.Errorf("portallocator: lease file %s is version %d, this build writes %d", a.path, f.Version, leaseFileVersion)
	}
	leases := make(map[leaseKey]Lease, len(f.Leases))
	for _, l := range f.Leases {
		if retiredPurposes[l.Purpose] {
			continue
		}
		leases[leaseKey{l.AppInstance, l.Cluster, l.Purpose}] = l
	}
	return leases, nil
}

type Request struct {
	Cluster  string
	Account  string
	Instance string
	Purpose  Purpose
}

func (a *Allocator) Allocate(ctx context.Context, req Request) (Lease, error) {
	if req.Cluster == "" {
		return Lease{}, errors.New("portallocator: Cluster is required; the lease is keyed on CR presence")
	}
	if req.Purpose.Width() == 0 {
		return Lease{}, fmt.Errorf("portallocator: unknown Purpose %q", req.Purpose)
	}
	a.mu.Lock()
	holder := a.holder
	a.mu.Unlock()
	if holder == nil {
		return Lease{}, ErrNoHolder
	}
	port, err := holder(ctx, req.Cluster, req.Purpose)
	if err != nil {
		return Lease{}, fmt.Errorf("portallocator: holding %s of %s: %w", req.Purpose, req.Cluster, err)
	}
	if port <= 0 || port > 65535 {
		return Lease{}, fmt.Errorf("portallocator: the holder answered port %d for %s of %s", port, req.Purpose, req.Cluster)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	var granted Lease
	err = a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		key := leaseKey{a.app, req.Cluster, req.Purpose}
		lease, ok := a.leases[key]
		if !ok || lease.Port != port {
			lease = Lease{Port: port, Width: 1, Purpose: req.Purpose, Cluster: req.Cluster, Account: req.Account,
				AppInstance: a.app, AllocatedAt: a.now()}
		}
		lease.Instance = req.Instance
		for k, other := range a.leases {
			if k != key && k.app == a.app && other.Covers(port) {
				delete(a.leases, k)
			}
		}
		a.leases[key] = lease
		if err := a.save(); err != nil {
			return err
		}
		granted = lease
		return nil
	})
	if err != nil {
		return Lease{}, err
	}
	return granted, nil
}

func (a *Allocator) Relet(ctx context.Context, req Request) (Lease, bool, error) {
	held, ok := a.PortFor(req.Cluster, req.Purpose)
	l, err := a.Allocate(ctx, req)
	return l, ok && l.Port != held.Port, err
}

func (a *Allocator) Release(cluster string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		for key := range a.leases {
			if key.app == a.app && key.cluster == cluster {
				delete(a.leases, key)
			}
		}
		return a.save()
	})
}

func (a *Allocator) ReleasePurpose(cluster string, purpose Purpose) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		key := leaseKey{a.app, cluster, purpose}
		if _, ok := a.leases[key]; !ok {
			return nil
		}
		delete(a.leases, key)
		return a.save()
	})
}

func (a *Allocator) Leases() []Lease {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	return a.own()
}

func (a *Allocator) PortFor(cluster string, purpose Purpose) (Lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	l, ok := a.leases[leaseKey{a.app, cluster, purpose}]
	return l, ok
}

func (a *Allocator) refresh() {
	_ = a.withLock(hostos.LockShared, a.load)
}

func (a *Allocator) Rekey(legacy, key string) (int, error) {
	if legacy == "" || key == "" || legacy == key {
		return 0, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	moved := 0
	err := a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		for held, l := range a.leases {
			if held.app != a.app || l.Cluster != legacy {
				continue
			}
			if _, taken := a.leases[leaseKey{a.app, key, held.purpose}]; taken {
				continue
			}
			delete(a.leases, held)
			l.Cluster = key
			a.leases[leaseKey{a.app, key, held.purpose}] = l
			moved++
		}
		if moved == 0 {
			return nil
		}
		return a.save()
	})
	if err != nil {
		return 0, err
	}
	return moved, nil
}

func (a *Allocator) Reclaim(known map[string]bool) ([]Lease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var freed []Lease
	err := a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		freed = LeasesToReclaim(a.own(), known, a.now(), a.grace)
		for _, l := range freed {
			delete(a.leases, leaseKey{l.AppInstance, l.Cluster, l.Purpose})
		}
		if len(freed) == 0 {
			return nil
		}
		return a.save()
	})
	if err != nil {
		return nil, err
	}
	return freed, nil
}

func LeasesToReclaim(leases []Lease, known map[string]bool, now time.Time, grace time.Duration) []Lease {
	var out []Lease
	for _, l := range leases {
		if known[l.Cluster] {
			continue
		}
		if grace > 0 && now.Sub(l.AllocatedAt) < grace {
			continue
		}
		out = append(out, l)
	}
	return out
}

func (a *Allocator) own() []Lease {
	var out []Lease
	for _, l := range a.snapshot() {
		if l.AppInstance == a.app {
			out = append(out, l)
		}
	}
	return out
}

func (a *Allocator) snapshot() []Lease {
	out := make([]Lease, 0, len(a.leases))
	for _, l := range a.leases {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Cluster < out[j].Cluster
	})
	return out
}

func (a *Allocator) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return fmt.Errorf("portallocator: lease dir: %w", err)
	}
	data, err := json.MarshalIndent(leaseFile{Version: leaseFileVersion, Leases: a.snapshot()}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("portallocator: lease file: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("portallocator: lease file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("portallocator: lease file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("portallocator: lease file: %w", err)
	}
	return os.Rename(tmp, a.path)
}

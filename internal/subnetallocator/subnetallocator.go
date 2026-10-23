package subnetallocator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
)

const (
	PoolBase = "10.100.0.0/16"

	LeaseBits = 24

	leaseFileVersion = 1

	leaseDirMode  = 0o700
	leaseFileMode = 0o600
)

func DefaultPool() Pool {
	return Pool{Base: netip.MustParsePrefix(PoolBase), Bits: LeaseBits}
}

type Pool struct {
	Base netip.Prefix
	Bits int
}

func (p Pool) Validate() error {
	if !p.Base.IsValid() {
		return fmt.Errorf("subnetallocator: pool %v is not a CIDR block, so there is no space to walk and "+
			"no way to compare two ranges for overlap; a pool that cannot be parsed is a pool that "+
			"silently contains everything", p.Base)
	}
	if !p.Base.Addr().Is4() {
		return fmt.Errorf("subnetallocator: pool %s is not IPv4. Every range this package leases "+
			"addresses a tap behind pasta: the guest's address, its netmask and the gateway it dials "+
			"this host through are the three values pasta's -a, -n and -g carry, and those are IPv4 "+
			"values in a firecracker guest whose kernel is booted with no IPv6 configuration of ours. "+
			"A pool of the wrong family leases ranges nothing can be addressed in", p.Base)
	}
	if p.Bits < p.Base.Bits() {
		return fmt.Errorf("subnetallocator: pool %s cannot be cut into /%d ranges, because a range "+
			"wider than the pool it comes from is not inside the pool. Every range must sit wholly "+
			"inside the pool, since the disjointness of two ranges is proved by the disjointness of "+
			"their blocks and that proof needs both of them enumerated over the same space",
			p.Base, p.Bits)
	}
	if p.Bits > 30 {
		return fmt.Errorf("subnetallocator: pool %s cannot be cut into /%d ranges: a /%d holds %d "+
			"addresses and a range needs one for the gateway pasta hands the guest as its default "+
			"route and at least one for a node, after the network and broadcast addresses are taken "+
			"out. The widest range that leaves a node an address is a /30",
			p.Base, p.Bits, p.Bits, 1<<uint(32-p.Bits))
	}
	return nil
}

func (p Pool) Masked() netip.Prefix { return p.Base.Masked() }

func (p Pool) BlockSize() uint64 {
	if err := p.Validate(); err != nil {
		return 0
	}
	return uint64(1) << uint(32-p.Bits)
}

func (p Pool) Blocks() uint64 {
	if err := p.Validate(); err != nil {
		return 0
	}
	return uint64(1) << uint(p.Bits-p.Base.Bits())
}

func (p Pool) BlockAt(i uint64) netip.Prefix {
	return netip.PrefixFrom(addOffset(p.Masked().Addr(), uint32(i*p.BlockSize())), p.Bits)
}

func (p Pool) Contains(block netip.Prefix) bool {
	if err := p.Validate(); err != nil {
		return false
	}
	if !block.IsValid() || !block.Addr().Is4() {
		return false
	}
	if block.Bits() < p.Base.Bits() {
		return false
	}
	return p.Masked().Contains(block.Masked().Addr())
}

func (p Pool) String() string {
	blocks := p.Blocks()
	if blocks == 1 {
		return fmt.Sprintf("%s, a single range of /%d", p.Masked(), p.Bits)
	}
	return fmt.Sprintf("%s, %d ranges of /%d", p.Masked(), blocks, p.Bits)
}

func addOffset(addr netip.Addr, off uint32) netip.Addr {
	v := addr.As4()
	x := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	x += off
	return netip.AddrFrom4([4]byte{byte(x >> 24), byte(x >> 16), byte(x >> 8), byte(x)})
}

type Lease struct {
	Cluster     string
	AppInstance string
	Block       netip.Prefix
	AllocatedAt time.Time
}

func (l Lease) Gateway() netip.Addr {
	return addOffset(l.Block.Masked().Addr(), 1)
}

func (l Lease) PrefixBits() int { return l.Block.Bits() }

func (l Lease) Netmask() netip.Addr {
	ones := l.Block.Bits()
	var v uint32
	if ones > 0 && ones < 32 {
		v = ^uint32(0) << uint(32-ones)
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func (l Lease) HostCount() int {
	if !l.Block.IsValid() || !l.Block.Addr().Is4() {
		return 0
	}
	return (1 << uint(32-l.Block.Bits())) - 2
}

func (l Lease) Host(n int) (netip.Addr, error) {
	if !l.Block.IsValid() || !l.Block.Addr().Is4() {
		return netip.Addr{}, fmt.Errorf("subnetallocator: cluster %s holds %v, which is not an IPv4 "+
			"range, so no guest address can be taken from it", l.Cluster, l.Block)
	}
	count := l.HostCount()
	if n < 1 || n > count {
		return netip.Addr{}, fmt.Errorf("subnetallocator: cluster %s's range %s holds %d usable "+
			"addresses numbered 1 to %d, where 1 is the gateway pasta hands the guest as its default "+
			"route and the rest are the nodes; %d is outside that range. The nodes of one cluster are "+
			"the only addresses that range will ever hold, so an address taken from outside it is a "+
			"guest sharing an address with a node of another cluster or with this host",
			l.Cluster, l.Block.Masked(), count, count, n)
	}
	return addOffset(l.Block.Masked().Addr(), uint32(n)), nil
}

func (l Lease) Contains(a netip.Addr) bool { return l.Block.Masked().Contains(a) }

func (l Lease) String() string {
	return fmt.Sprintf("%s held by cluster %s", l.Block.Masked(), l.Cluster)
}

type Request struct {
	Cluster string
}

type Config struct {
	LeasePath string

	AppInstance string

	HostNetworks func() ([]HostNetwork, error)

	Now func() time.Time
}

type leaseKey struct {
	app     string
	cluster string
}

type Allocator struct {
	app   string
	path  string
	pool  Pool
	hosts func() ([]HostNetwork, error)
	now   func() time.Time

	mu     sync.Mutex
	leases map[leaseKey]Lease
}

type leaseRecord struct {
	Cluster     string    `json:"cluster"`
	AppInstance string    `json:"appInstance,omitempty"`
	Block       string    `json:"block"`
	AllocatedAt time.Time `json:"allocatedAt"`
}

type leaseFile struct {
	Version int           `json:"version"`
	Pool    string        `json:"pool"`
	Leases  []leaseRecord `json:"leases"`
}

func Open(cfg Config) (*Allocator, error) {
	return openWithPool(cfg, DefaultPool())
}

func openWithPool(cfg Config, pool Pool) (*Allocator, error) {
	if cfg.LeasePath == "" {
		return nil, errors.New("subnetallocator: LeasePath is required. The host-side lease file is " +
			"the only record of which cluster holds which range, so a process that names none leases " +
			"from a pool with no memory: it hands the same range out twice, and the second cluster's " +
			"nodes are addressed exactly as the first cluster's nodes with nothing in either cluster " +
			"reporting it")
	}
	if err := pool.Validate(); err != nil {
		return nil, err
	}
	a := &Allocator{
		app:    cfg.AppInstance,
		path:   cfg.LeasePath,
		pool:   pool,
		hosts:  cfg.HostNetworks,
		now:    cfg.Now,
		leases: map[leaseKey]Lease{},
	}
	if a.hosts == nil {
		a.hosts = HostNetworks
	}
	if a.now == nil {
		a.now = time.Now
	}
	if _, err := a.hosts(); err != nil {
		return nil, fmt.Errorf("subnetallocator: enumerating the networks this host carries, before "+
			"this gateway leases anything: %w. Every range this package hands out is non-conflicting "+
			"only because it was compared against the host's own networks, so a host whose networks "+
			"cannot be read is a host against which no range can be checked. Refusing here rather "+
			"than at the first allocation, because a gateway that starts is a gateway an operator "+
			"believes, and this one would start on the strength of an enumeration that failed", err)
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Allocator) Pool() Pool { return a.pool }

func (a *Allocator) lockPath() string { return a.path + ".lock" }

func (a *Allocator) withLock(how int, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(a.path), leaseDirMode); err != nil {
		return fmt.Errorf("subnetallocator: lease dir: %w", err)
	}
	f, err := os.OpenFile(a.lockPath(), os.O_RDWR|os.O_CREATE, leaseFileMode)
	if err != nil {
		return fmt.Errorf("subnetallocator: lease lock %s: %w", a.lockPath(), err)
	}
	defer f.Close()
	if err := hostos.Lock(f, how); err != nil {
		return fmt.Errorf("subnetallocator: locking %s: %w", a.lockPath(), err)
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
		return nil, fmt.Errorf("subnetallocator: reading the lease file %s: %w", a.path, err)
	}
	var f leaseFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("subnetallocator: the lease file %s is unreadable, refusing to start "+
			"with an empty store over it: %w. Starting empty does not fail -- it quietly leases a "+
			"SECOND range to every cluster this file names, and the range each of those clusters is "+
			"already addressed in is then held by nobody, so the next cluster that walks the pool is "+
			"handed a range live nodes are still in", a.path, err)
	}
	if f.Version != leaseFileVersion {
		return nil, fmt.Errorf("subnetallocator: the lease file %s is version %d and this build "+
			"writes %d; refusing to read it, because the fields this build does not know about are "+
			"the ranges it would fail to see and hand out a second time",
			a.path, f.Version, leaseFileVersion)
	}
	if f.Pool != a.pool.Masked().String() {
		return nil, fmt.Errorf("subnetallocator: the lease file %s records ranges leased from pool "+
			"%s, and this build leases from pool %s. Refusing to start, because the two are different "+
			"spaces: continuing would walk the new pool while the record still holds blocks of the old "+
			"one, so a range already leased would not be seen and would be handed to a second cluster. "+
			"The pool is a constant in this package's code, so a change here is a migration: move or "+
			"release the leases in that file by hand, with the clusters they belong to named in it",
			a.path, f.Pool, a.pool.Masked())
	}
	leases := make(map[leaseKey]Lease, len(f.Leases))
	for _, r := range f.Leases {
		if r.Cluster == "" {
			return nil, fmt.Errorf("subnetallocator: the lease file %s records a range %q held by no "+
				"cluster. A range is held by a cluster or it is free; one held by nothing can neither "+
				"be leased again safely nor released, and it addresses nodes that are still running "+
				"in it, so this refuses to start rather than walk the pool over it",
				a.path, r.Block)
		}
		block, err := netip.ParsePrefix(r.Block)
		if err != nil {
			return nil, fmt.Errorf("subnetallocator: the lease file %s records %q for cluster %s, "+
				"which is not a CIDR block: %w. Refusing to start over a store whose addresses cannot "+
				"be read, because the alternative is leasing a SECOND range to a cluster that already "+
				"holds one and moving its nodes' addresses out from under them",
				a.path, r.Block, r.Cluster, err)
		}
		if !block.Addr().Is4() || block.Bits() != a.pool.Bits {
			return nil, fmt.Errorf("subnetallocator: the lease file %s records %s for cluster %s, "+
				"which is not a /%d IPv4 block. A lease is a range of the width this pool is cut "+
				"into, so a range of another width is one that was never disjoint from a range of "+
				"this one: it may overlap a block another cluster holds without this package being "+
				"able to see it", a.path, block, r.Cluster, a.pool.Bits)
		}
		block = block.Masked()
		if !a.pool.Contains(block) {
			return nil, fmt.Errorf("subnetallocator: the lease file %s records %s for cluster %s, "+
				"which is outside pool %s. A range outside the pool is a range the walk will never "+
				"look at and never reconcile, so the block it occupies inside the pool if it "+
				"overlaps one would be leased again to a second cluster and both would address "+
				"their nodes alike. Refusing to start rather than leasing around a record that "+
				"covers ground this build cannot see", a.path, block, r.Cluster, a.pool.Masked())
		}
		if prev, ok := leases[leaseKey{r.AppInstance, r.Cluster}]; ok {
			return nil, fmt.Errorf("subnetallocator: the lease file %s names cluster %s twice, at %s "+
				"and at %s. One of those two ranges is held by nobody and the pool walk will hand it "+
				"to the next cluster, whose nodes are then addressed inside a range this cluster's "+
				"nodes may still be in. Refusing to start rather than choosing one of the two, "+
				"because the file is the only evidence of which range the cluster's nodes are "+
				"actually addressed in and a guess here renumbers a running cluster",
				a.path, r.Cluster, prev.Block, block)
		}
		leases[leaseKey{r.AppInstance, r.Cluster}] = Lease{Cluster: r.Cluster, AppInstance: r.AppInstance, Block: block, AllocatedAt: r.AllocatedAt}
	}
	return leases, nil
}

func (a *Allocator) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), leaseDirMode); err != nil {
		return fmt.Errorf("subnetallocator: lease dir: %w", err)
	}
	records := make([]leaseRecord, 0, len(a.leases))
	for _, l := range a.snapshot() {
		records = append(records, leaseRecord{
			Cluster:     l.Cluster,
			AppInstance: l.AppInstance,
			Block:       l.Block.Masked().String(),
			AllocatedAt: l.AllocatedAt,
		})
	}
	data, err := json.MarshalIndent(leaseFile{
		Version: leaseFileVersion,
		Pool:    a.pool.Masked().String(),
		Leases:  records,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, leaseFileMode)
	if err != nil {
		return fmt.Errorf("subnetallocator: lease file: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("subnetallocator: lease file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("subnetallocator: lease file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("subnetallocator: lease file: %w", err)
	}
	return os.Rename(tmp, a.path)
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
		held, ok := a.leases[leaseKey{a.app, legacy}]
		if !ok {
			return nil
		}
		if _, taken := a.leases[leaseKey{a.app, key}]; taken {
			return nil
		}
		delete(a.leases, leaseKey{a.app, legacy})
		held.Cluster = key
		a.leases[leaseKey{a.app, key}] = held
		moved = 1
		return a.save()
	})
	if err != nil {
		return 0, err
	}
	return moved, nil
}

func (a *Allocator) Allocate(req Request) (Lease, error) {
	if req.Cluster == "" {
		return Lease{}, errors.New("subnetallocator: Cluster is required. A range is leased to a " +
			"cluster and looked up by that name on every later start, so an unnamed lease can never " +
			"be found again: the same cluster on its next start would be handed a second range and " +
			"its nodes would move out of the first one while that range stayed leased to nobody")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	var granted Lease
	err := a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}

		if found := FindDuplicateBlocks(a.snapshot()); len(found) > 0 {
			return errors.New(strings.Join(found, "; "))
		}

		if held, ok := a.leases[leaseKey{a.app, req.Cluster}]; ok {
			granted = held
			return nil
		}

		hosts, err := a.hosts()
		if err != nil {
			return fmt.Errorf("subnetallocator: enumerating the networks this host carries, before "+
				"leasing a range to cluster %s: %w. Every range handed out here is non-conflicting "+
				"only because it was compared against the host's own networks, so leasing on an "+
				"enumeration that failed is leasing a range nobody checked. Nothing is leased and "+
				"nothing is written", req.Cluster, err)
		}

		blocks := a.pool.Blocks()
		exhausted := &PoolExhaustedError{Pool: a.pool, Candidates: blocks}
		for i := range blocks {
			candidate := a.pool.BlockAt(i)
			err := a.checkLocked(candidate, hosts)

			var hostErr *HostCollisionError
			var leaseErr *LeaseCollisionError
			switch {
			case err == nil:
				lease := Lease{
					Cluster:     req.Cluster,
					AppInstance: a.app,
					Block:       candidate,
					AllocatedAt: a.now(),
				}
				a.leases[leaseKey{a.app, req.Cluster}] = lease
				if saveErr := a.save(); saveErr != nil {
					delete(a.leases, leaseKey{a.app, req.Cluster})
					return saveErr
				}
				granted = lease
				return nil
			case errors.As(err, &hostErr):
				exhausted.HostSkips++
				if exhausted.HostFirst == nil {
					exhausted.HostFirst = hostErr
				}
			case errors.As(err, &leaseErr):
				exhausted.LeaseSkips++
				if exhausted.LeaseFirst == nil {
					exhausted.LeaseFirst = leaseErr
				}
			default:
				return err
			}
		}
		return exhausted
	})
	if err != nil {
		return Lease{}, err
	}
	return granted, nil
}

func (a *Allocator) Check(block netip.Prefix) error {
	hosts, err := a.hosts()
	if err != nil {
		return fmt.Errorf("subnetallocator: enumerating the networks this host carries, before "+
			"checking %s: %w. A range is non-conflicting only against an enumeration that succeeded, "+
			"so this reports the enumeration's failure rather than the check it could not do", block, err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	return a.checkLocked(block, hosts)
}

func (a *Allocator) Release(cluster string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.withLock(hostos.LockExclusive, func() error {
		if err := a.load(); err != nil {
			return err
		}
		if _, ok := a.leases[leaseKey{a.app, cluster}]; !ok {
			return nil
		}
		delete(a.leases, leaseKey{a.app, cluster})
		return a.save()
	})
}

func (a *Allocator) AllLeases() []Lease {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	return a.snapshot()
}

func (a *Allocator) Leases() []Lease {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	var out []Lease
	for _, l := range a.snapshot() {
		if l.AppInstance == a.app {
			out = append(out, l)
		}
	}
	return out
}

func (a *Allocator) LeaseFor(cluster string) (Lease, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	l, ok := a.leases[leaseKey{a.app, cluster}]
	return l, ok
}

func (a *Allocator) refresh() {
	_ = a.withLock(hostos.LockShared, a.load)
}

func (a *Allocator) snapshot() []Lease {
	out := make([]Lease, 0, len(a.leases))
	for _, l := range a.leases {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Block != out[j].Block {
			return lessBlock(out[i].Block, out[j].Block)
		}
		return out[i].Cluster < out[j].Cluster
	})
	return out
}

func lessBlock(a, b netip.Prefix) bool {
	x, y := a.Masked().Addr(), b.Masked().Addr()
	if !x.Is4() || !y.Is4() {
		return x.String() < y.String()
	}
	xa, ya := x.As4(), y.As4()
	for i := range 4 {
		if xa[i] != ya[i] {
			return xa[i] < ya[i]
		}
	}
	return false
}

package liveports

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
)

type Block struct {
	What  string
	First int
	Slots int
	Width int
}

type Span struct {
	What  string
	First int
	Width int
}

const (
	GuestJoinProbeFirst = 64000
	GuestJoinProbeWidth = 256
)

var (
	ZeroNodeProof   = Block{What: "the zero-node proof", First: 30500, Slots: 8, Width: 100}
	PostgresWindows = Block{What: "the postgres windows", First: 31300, Slots: 8, Width: 100}
	MountProof      = Block{What: "the kcp mount proof", First: 32100, Slots: 4, Width: 160}
	RebuildProof    = Block{What: "the rebuild proof", First: 61000, Slots: 8, Width: 10}
	FourStepProof   = Block{What: "the four-step proof", First: 62000, Slots: 8, Width: 100}
	SubnetProof     = Block{What: "the subnet proof", First: 63000, Slots: 8, Width: 100}
	SchemaUpgrade   = Block{What: "the schema upgrade proof", First: 63800, Slots: 4, Width: 10}

	Blocks = []Block{ZeroNodeProof, PostgresWindows, MountProof, RebuildProof, FourStepProof, SubnetProof, SchemaUpgrade}

	GuestJoinProbe = Span{What: "the guest-join probe", First: GuestJoinProbeFirst, Width: GuestJoinProbeWidth}

	Probed = []Span{GuestJoinProbe}

	DefaultCarve = Span{
		What:  "the composition root's default carve",
		First: portallocator.DefaultFirst,
		Width: portallocator.DefaultLast - portallocator.DefaultFirst + 1,
	}

	DevCarve = Span{
		What:  "the per-user dev lease carve",
		First: portallocator.DevFirst,
		Width: portallocator.DevLast - portallocator.DevFirst + 1,
	}

	Reserved = []Span{DefaultCarve, DevCarve}
)

var ErrNoSlot = errors.New(
	"liveports: every slot of this block is held by another process on this host. A block is for a " +
		"test that binds real ports, so two runs sharing a slot collide on the store and on the " +
		"apiserver -- the loser fails its readiness deadline rather than its bind, and that reads as a " +
		"broken control plane in whatever was committed last. Wait for the other run, or widen the " +
		"block's Slots")

var ErrNoLockDir = errors.New(
	"liveports: the slot lock directory is unusable. Two runs cannot see each other without it, and " +
		"a run that cannot see its peers takes a slot a peer is already binding")

func LockDir() string { return "/var/tmp/socialweb-liveports" }

func (b Block) Span() portallocator.Range {
	return portallocator.Range{First: b.First, Last: b.First + b.Slots*b.Width - 1}
}

func (b Block) Slot(i int) portallocator.Range {
	first := b.First + i*b.Width
	return portallocator.Range{First: first, Last: first + b.Width - 1}
}

func (s Span) Span() portallocator.Range {
	return portallocator.Range{First: s.First, Last: s.First + s.Width - 1}
}

func Text(r portallocator.Range) string {
	return strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last)
}

func lockPath(b Block, i int) string {
	return filepath.Join(LockDir(), strconv.Itoa(b.First)+"-"+strconv.Itoa(i)+".lock")
}

func TakeForProcess(b Block) (portallocator.Range, *os.File, error) {
	if err := os.MkdirAll(LockDir(), 0o700); err != nil {
		return portallocator.Range{}, nil, fmt.Errorf("%w: %s: %v", ErrNoLockDir, LockDir(), err)
	}
	start := os.Getpid() % b.Slots
	for n := 0; n < b.Slots; n++ {
		i := (start + n) % b.Slots
		f, err := os.OpenFile(lockPath(b, i), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return portallocator.Range{}, nil, fmt.Errorf("%w: %s: %v", ErrNoLockDir, lockPath(b, i), err)
		}
		if err := hostos.Lock(f, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
			_ = f.Close()
			continue
		}
		return b.Slot(i), f, nil
	}
	return portallocator.Range{}, nil, fmt.Errorf("%w (%s, %d slots from %d)", ErrNoSlot, b.What, b.Slots, b.First)
}

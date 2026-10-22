package table

import (
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

type Kind string

const (
	KindStore Kind = "store"
	KindKine  Kind = "kine"
	KindFront Kind = "front"
	KindSlot  Kind = "slot"
	KindCore  Kind = "core"
)

const (
	RoleGateway   = "gateway"
	RoleBidder    = "bidder"
	StoreSqlite   = "kine-sqlite"
	StorePostgres = "kine-postgres"
	GatewayKey    = "gw"
	GatewayCore   = "computer-gateway" + hostsvc.ServiceSuffix
	BidderCore    = "computer-bidder@"
	RolePostgres  = "pg"
	RoleFront     = "front"
	RoleCore      = "core"
	SlotA         = "a"
	SlotB         = "b"
)

var (
	keyRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,22}[a-z0-9])?$`)
	slotRE = regexp.MustCompile(`^[a-z]$`)
)

type App struct {
	Role    string
	Key     string
	Handle  string
	Store   string
	Slots   []string
	DevCore bool
	Prefix  string
}

type Entry struct {
	Kind  Kind
	Role  string
	Unit  string
	Slot  string
	After []string
	Wants []string
}

type Names struct {
	Prefix string
	Key    string
}

func Gateway() App {
	return App{Role: RoleGateway, Key: GatewayKey, Store: StorePostgres, Slots: []string{SlotA, SlotB}}
}

func Bidder(handle, hid string) App {
	return App{Role: RoleBidder, Key: hid, Handle: handle, Store: StoreSqlite, Slots: []string{SlotA}}
}

func Dev(key, store string) App {
	slots := []string{SlotA}
	if store == StorePostgres {
		slots = []string{SlotA, SlotB}
	}
	return App{Role: RoleGateway, Key: key, Store: store, Slots: slots, DevCore: true}
}

func DevNames(key string) Names { return Names{Prefix: hostsvc.NamePrefix, Key: key} }

func (a App) Names() Names {
	p := a.Prefix
	if p == "" {
		p = hostsvc.NamePrefix
	}
	return Names{Prefix: p, Key: a.Key}
}

func (n Names) base() string { return n.Prefix + "kcp-" + n.Key }

func (n Names) Store() string { return n.base() + "-pg" + hostsvc.ServiceSuffix }

func (n Names) Kine(slot string) string { return n.base() + "-kine@" + slot + hostsvc.ServiceSuffix }

func (n Names) Front() string { return n.base() + "-front" + hostsvc.ServiceSuffix }

func (n Names) Slot(slot string) string { return n.base() + "@" + slot + hostsvc.ServiceSuffix }

func (n Names) Slice() string { return n.base() + ".slice" }

func (n Names) UnitPrefix() string { return n.base() }

func (n Names) DevCore() string { return n.Prefix + "core-" + n.Key + "-dev" + hostsvc.ServiceSuffix }

func (n Names) HostProcessPrefix() string { return n.Prefix + "hp-" + n.Key + "-" }

func (n Names) CorePrefix() string { return n.Prefix + "core-" + n.Key + "-" }

func (n Names) CoreSlice() string { return n.Prefix + "core-" + n.Key + ".slice" }

func (n Names) StandIn(role string) string { return n.base() + "-" + role + hostsvc.ServiceSuffix }

func KineRole(slot string) string { return "kine-" + slot }

func SlotRole(slot string) string { return "kcp-" + slot }

func (a App) Validate() error {
	if a.Role != RoleGateway && a.Role != RoleBidder {
		return fmt.Errorf("bootstrap: application role %q: want %s or %s", a.Role, RoleGateway, RoleBidder)
	}
	if !keyRE.MatchString(a.Key) {
		return fmt.Errorf("bootstrap: application key %q: want lower-case letters, digits and dashes, at most 24", a.Key)
	}
	if a.Store != StoreSqlite && a.Store != StorePostgres {
		return fmt.Errorf("bootstrap: store %q: want %s or %s", a.Store, StoreSqlite, StorePostgres)
	}
	if a.Role == RoleBidder {
		if a.Store != StoreSqlite {
			return errors.New("bootstrap: a bidder's kcp is one slot on kine-sqlite; it never has a postgres")
		}
		if a.Handle == "" && !a.DevCore {
			return errors.New("bootstrap: a bidder application names its handle")
		}
	}
	if len(a.Slots) == 0 || len(a.Slots) > 2 {
		return fmt.Errorf("bootstrap: %d kcp slots: want one or two", len(a.Slots))
	}
	if a.Role == RoleBidder && len(a.Slots) != 1 {
		return errors.New("bootstrap: a bidder runs one kcp slot")
	}
	for i, s := range a.Slots {
		if !slotRE.MatchString(s) || slices.Contains(a.Slots[:i], s) {
			return fmt.Errorf("bootstrap: slot %q: want distinct single letters", s)
		}
	}
	return nil
}

func (a App) KineSlots() []string {
	if a.Store == StorePostgres {
		return slices.Clone(a.Slots)
	}
	return []string{a.Slots[0]}
}

func (a App) KineOf(slot string) string {
	if a.Store == StorePostgres && slices.Contains(a.Slots, slot) {
		return slot
	}
	return a.Slots[0]
}

func (a App) SlotEntry(slot string) Entry {
	n := a.Names()
	kine := n.Kine(a.KineOf(slot))
	return Entry{Kind: KindSlot, Role: SlotRole(slot), Unit: n.Slot(slot), Slot: slot, After: []string{kine, n.Front()}, Wants: []string{kine}}
}

func (a App) CoreUnit() string {
	switch {
	case a.DevCore:
		return a.Names().DevCore()
	case a.Role == RoleBidder:
		return a.Names().Prefix + BidderCore + a.Handle + hostsvc.ServiceSuffix
	}
	return a.Names().Prefix + GatewayCore
}

func CoreTemplate(prefix, role string) string {
	if prefix == "" {
		prefix = hostsvc.NamePrefix
	}
	if role == RoleBidder {
		return prefix + BidderCore + hostsvc.ServiceSuffix
	}
	return prefix + GatewayCore
}

func Table(a App) ([]Entry, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	n := a.Names()
	var out []Entry
	var store []string
	if a.Store == StorePostgres {
		store = []string{n.Store()}
		out = append(out, Entry{Kind: KindStore, Role: RolePostgres, Unit: n.Store()})
	}
	for _, s := range a.KineSlots() {
		out = append(out, Entry{Kind: KindKine, Role: KineRole(s), Unit: n.Kine(s), Slot: s, After: store, Wants: store})
	}
	out = append(out, Entry{Kind: KindFront, Role: RoleFront, Unit: n.Front()})
	for _, s := range a.Slots {
		out = append(out, a.SlotEntry(s))
	}
	var after, wants []string
	for _, e := range out {
		after = append(after, e.Unit)
		if e.Kind != KindSlot {
			wants = append(wants, e.Unit)
		}
	}
	out = append(out, Entry{Kind: KindCore, Role: RoleCore, Unit: a.CoreUnit(), After: after, Wants: wants})
	return out, nil
}

func Order() []Kind { return []Kind{KindStore, KindKine, KindFront, KindSlot, KindCore} }

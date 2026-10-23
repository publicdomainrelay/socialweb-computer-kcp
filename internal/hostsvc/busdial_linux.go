//go:build linux

package hostsvc

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	sd "github.com/coreos/go-systemd/v22/dbus"
	"github.com/godbus/dbus/v5"
)

const (
	systemdDest = "org.freedesktop.systemd1"
	systemdPath = dbus.ObjectPath("/org/freedesktop/systemd1")
	managerIfc  = "org.freedesktop.systemd1.Manager"
)

type bus interface {
	StartTransientUnit(ctx context.Context, name string, props []sd.Property, ch chan<- string) error
	StopUnit(ctx context.Context, name string, ch chan<- string) error
	ResetFailedUnit(ctx context.Context, name string) error
	UnitProperties(ctx context.Context, name string) (map[string]any, error)
	ServiceProperties(ctx context.Context, name string) (map[string]any, error)
	ListUnitsByPatterns(ctx context.Context, patterns []string) ([]sd.UnitStatus, error)
	UnitByPIDFD(ctx context.Context, fd int) (string, string, error)
	Version(ctx context.Context) (string, error)
	ManagerStarted(ctx context.Context) (uint64, error)
	Subscribe(ctx context.Context, ch chan<- *dbus.Signal) error
	Close()
}

type dialError struct{ err error }

func (e dialError) Error() string { return "hostsvc: dialing the user manager: " + e.err.Error() }

func (e dialError) Unwrap() error { return e.err }

func PrivateSocketPath() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	return filepath.Join(dir, "systemd", "private")
}

func dialPrivate(path string) func(ctx context.Context) (bus, error) {
	return func(ctx context.Context) (bus, error) {
		var mu sync.Mutex
		var conns []*dbus.Conn
		dialOne := func() (*dbus.Conn, error) {
			c, err := dbus.Dial("unix:path=" + path)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			if err := ctx.Err(); err != nil {
				c.Close()
				return nil, err
			}
			if err := c.Auth([]dbus.Auth{dbus.AuthExternal(strconv.Itoa(os.Getuid()))}); err != nil {
				c.Close()
				return nil, err
			}
			return c, nil
		}
		watchdog := context.AfterFunc(ctx, func() {
			mu.Lock()
			defer mu.Unlock()
			for _, c := range conns {
				c.Close()
			}
		})
		conn, err := sd.NewConnection(dialOne)
		if !watchdog() {
			if conn != nil {
				conn.Close()
			}
			return nil, dialError{fmt.Errorf("%s: %w", path, ctx.Err())}
		}
		if err != nil {
			return nil, dialError{fmt.Errorf("%s: %w", path, err)}
		}
		return &sdBus{conn: conn, raw: conns[0], sig: conns[1]}, nil
	}
}

type sdBus struct {
	conn *sd.Conn
	raw  *dbus.Conn
	sig  *dbus.Conn
}

func (b *sdBus) StartTransientUnit(ctx context.Context, name string, props []sd.Property, ch chan<- string) error {
	_, err := b.conn.StartTransientUnitContext(ctx, name, "fail", props, ch)
	return err
}

func (b *sdBus) StartUnit(ctx context.Context, name string, ch chan<- string) error {
	_, err := b.conn.StartUnitContext(ctx, name, "replace", ch)
	return err
}

func (b *sdBus) EnableUnitFiles(ctx context.Context, names []string) error {
	_, _, err := b.conn.EnableUnitFilesContext(ctx, names, false, false)
	return err
}

func (b *sdBus) DisableUnitFiles(ctx context.Context, names []string) error {
	_, err := b.conn.DisableUnitFilesContext(ctx, names, false)
	return err
}

func (b *sdBus) StopUnit(ctx context.Context, name string, ch chan<- string) error {
	_, err := b.conn.StopUnitContext(ctx, name, "replace", ch)
	return err
}

func (b *sdBus) ResetFailedUnit(ctx context.Context, name string) error {
	return b.conn.ResetFailedUnitContext(ctx, name)
}

func (b *sdBus) UnitProperties(ctx context.Context, name string) (map[string]any, error) {
	return b.conn.GetUnitPropertiesContext(ctx, name)
}

func (b *sdBus) ServiceProperties(ctx context.Context, name string) (map[string]any, error) {
	return b.conn.GetUnitTypePropertiesContext(ctx, name, "Service")
}

func (b *sdBus) ListUnitsByPatterns(ctx context.Context, patterns []string) ([]sd.UnitStatus, error) {
	return b.conn.ListUnitsByPatternsContext(ctx, nil, patterns)
}

func (b *sdBus) UnitByPIDFD(ctx context.Context, fd int) (string, string, error) {
	var path dbus.ObjectPath
	var name string
	var inv []byte
	err := b.raw.Object(systemdDest, systemdPath).
		CallWithContext(ctx, managerIfc+".GetUnitByPIDFD", 0, dbus.UnixFD(fd)).
		Store(&path, &name, &inv)
	if err != nil {
		return "", "", err
	}
	return name, hex.EncodeToString(inv), nil
}

func (b *sdBus) Version(ctx context.Context) (string, error) {
	var v dbus.Variant
	err := b.raw.Object(systemdDest, systemdPath).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, managerIfc, "Version").
		Store(&v)
	if err != nil {
		return "", err
	}
	s, ok := v.Value().(string)
	if !ok {
		return "", fmt.Errorf("hostsvc: the manager's Version is a %T, not a string", v.Value())
	}
	return s, nil
}

func (b *sdBus) ManagerStarted(ctx context.Context) (uint64, error) {
	var v dbus.Variant
	err := b.raw.Object(systemdDest, systemdPath).
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, managerIfc, "UserspaceTimestampMonotonic").
		Store(&v)
	if err != nil {
		return 0, err
	}
	n, ok := v.Value().(uint64)
	if !ok {
		return 0, fmt.Errorf("hostsvc: the manager's UserspaceTimestampMonotonic is a %T, not a uint64", v.Value())
	}
	return n, nil
}

func (b *sdBus) Subscribe(ctx context.Context, ch chan<- *dbus.Signal) error {
	b.sig.Signal(ch)
	return b.sig.Object(systemdDest, systemdPath).CallWithContext(ctx, managerIfc+".Subscribe", 0).Err
}

func (b *sdBus) Close() {
	b.conn.Close()
}

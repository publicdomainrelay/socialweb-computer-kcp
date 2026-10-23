package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

type Placed struct {
	Role   string             `json:"role"`
	Unit   string             `json:"unit"`
	Status hostsvc.UnitStatus `json:"status"`
	Port   int                `json:"port,omitempty"`
	At     time.Time          `json:"at"`
}

func Bootable(a table.App) ([]table.Entry, error) {
	tbl, err := table.Table(a)
	if err != nil {
		return nil, err
	}
	first := a.Slots[0]
	var out []table.Entry
	for _, e := range tbl {
		switch e.Kind {
		case table.KindKine:
			if e.Slot != a.KineOf(first) {
				continue
			}
		case table.KindSlot:
			if e.Slot != first {
				continue
			}
		case table.KindCore:
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func retarget(link, target string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		return err
	}
	tmp := link + ".new"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

func LinkAssets(p Params, kcp, kine, postgresRoot string) error {
	links := map[string]string{p.KcpPath: kcp, p.KinePath: kine}
	if p.App.Store == table.StorePostgres {
		if postgresRoot == "" {
			return errors.New("bootstrap: a postgres store needs the extracted postgres asset tree")
		}
		links[p.PostgresRoot] = postgresRoot
	}
	for link, target := range links {
		if !filepath.IsAbs(target) {
			return fmt.Errorf("bootstrap: asset %s must be an absolute path", target)
		}
		if err := retarget(link, target); err != nil {
			return err
		}
	}
	return nil
}

func WriteEnvFiles(p Params, frontURL string, overwrite bool) ([]string, error) {
	units, err := Installed(p, frontURL)
	if err != nil {
		return nil, err
	}
	var written []string
	for _, u := range units {
		if u.File.EnvPath == "" || u.File.Env == nil {
			continue
		}
		if !overwrite {
			if _, err := os.Stat(u.File.EnvPath); err == nil {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(u.File.EnvPath), 0o700); err != nil {
			return written, err
		}
		if err := os.WriteFile(u.File.EnvPath, u.File.Env, 0o600); err != nil {
			return written, err
		}
		written = append(written, u.File.EnvPath)
	}
	return written, nil
}

func StartLayer(ctx context.Context, m hostsvc.Manager, p Params) ([]Placed, error) {
	entries, err := Bootable(p.App)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{p.Dir, p.Run, p.PgSocketDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	timeout := p.ReadyTimeout
	timeout = opt.Positive(timeout, DefaultReadyTimeout)
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out []Placed
	for _, e := range entries {
		if e.Kind == table.KindKine {
			if err := os.MkdirAll(p.KineDir(e.Slot), 0o700); err != nil {
				return out, err
			}
		}
		if e.Kind == table.KindStore {
			if err := initdb(ctx, p); err != nil {
				return out, err
			}
		}
		if e.Kind == table.KindSlot {
			if err := kcpserver.WriteSlotFiles(p.SlotRoot(e.Slot)); err != nil {
				return out, err
			}
		}
		st, err := hostsvc.StartInstalled(ctx, m, e.Unit)
		if err != nil {
			return out, err
		}
		pl := Placed{Role: e.Role, Unit: e.Unit, Status: st, At: time.Now().UTC()}
		switch e.Kind {
		case table.KindStore:
			err = waitPostgres(rctx, p)
		case table.KindKine:
			err = waitUnixSocket(rctx, p.KineSocket(e.Slot))
		case table.KindFront, table.KindSlot:
			pl.Port, err = hostsvc.WaitListenPort(rctx, st.MainPID, PortPoll)
		}
		if err != nil {
			return out, fmt.Errorf("bootstrap: %s: %w", e.Unit, err)
		}
		out = append(out, pl)
	}
	return out, nil
}

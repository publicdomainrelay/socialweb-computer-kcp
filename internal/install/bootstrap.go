package install

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
)

type Enabler interface {
	Enable(ctx context.Context, units []string) error
}

func BootstrapApps(role hostcontract.Role, prefix string, handles []string) []table.App {
	if role == hostcontract.RoleGateway {
		g := table.Gateway()
		g.Prefix = prefix
		return []table.App{g}
	}
	var out []table.App
	if role == hostcontract.RoleRequester {
		for _, n := range handles {
			r := table.Requester(n, RequesterKey(n))
			r.Prefix = prefix
			out = append(out, r)
		}
		return out
	}
	for _, h := range handles {
		b := table.Bidder(h, HandleID(h))
		b.Prefix = prefix
		out = append(out, b)
	}
	return out
}

func BootableUnits(role hostcontract.Role, prefix string, handles []string) ([]string, error) {
	var out []string
	for _, a := range BootstrapApps(role, prefix, handles) {
		entries, err := bootstrap.Bootable(a)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			out = append(out, e.Unit)
		}
	}
	return out, nil
}

func (in *installer) bootstrapFiles(dir string) ([]string, bool, error) {
	var names []string
	changed := false
	uid, gid := in.acct.UID, in.acct.GID
	front := in.configString("kcp-front-address")
	for _, a := range BootstrapApps(in.o.Role, in.o.UnitPrefix, in.instances()) {
		p := bootstrap.InstallParams(a, in.o.StateDir)
		frontURL := ""
		if front != "" && a.Role == table.RoleGateway {
			p.FrontAddress = front
			if !strings.HasSuffix(front, ":0") {
				frontURL = "https://" + front
			}
		}
		units, err := bootstrap.Installed(p, frontURL)
		if err != nil {
			return nil, false, err
		}
		for _, d := range append(in.accountParents(p.Dir), p.Dir, p.Run, filepath.Join(p.Dir, "bin")) {
			if err := in.ensureDir(d, 0o700, uid, gid, false); err != nil {
				return nil, false, err
			}
		}
		for _, u := range units {
			full := filepath.Join(dir, u.Path)
			if strings.Contains(u.Path, "/") {
				if err := in.ensureDir(filepath.Dir(full), 0o755, uid, gid, false); err != nil {
					return nil, false, err
				}
			}
			c, err := in.ensureFile(full, u.File.Unit, 0o644, uid, gid)
			if err != nil {
				return nil, false, err
			}
			in.rec.addUnitFile(full)
			if c {
				in.noteWritten(u.Path)
			}
			changed = changed || c
			names = append(names, u.Path)
			if u.File.EnvPath == "" || u.File.Env == nil {
				continue
			}
			if u.Entry.Kind == table.KindSlot {
				if _, err := os.Stat(in.path(u.File.EnvPath)); err == nil {
					continue
				}
			}
			ec, err := in.ensureFile(u.File.EnvPath, u.File.Env, 0o600, uid, gid)
			if err != nil {
				return nil, false, err
			}
			changed = changed || ec
		}
	}
	return names, changed, nil
}

func (in *installer) configString(key string) string {
	b, err := os.ReadFile(in.path(filepath.Join(in.o.StateDir, "config.json")))
	if err != nil {
		return ""
	}
	cfg := map[string]any{}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	s, _ := cfg[key].(string)
	return s
}

func (in *installer) enableBootstrap(ctx context.Context, um UserManager) error {
	units, err := BootableUnits(in.o.Role, in.o.UnitPrefix, in.instances())
	if err != nil || len(units) == 0 {
		return err
	}
	e, ok := um.(Enabler)
	if !ok {
		return nil
	}
	return in.change("enable "+strings.Join(units, " ")+" as "+in.o.Account+" (started at boot, before the core)", func() error {
		return e.Enable(ctx, units)
	})
}

func (in *installer) accountParents(dir string) []string {
	var out []string
	for d := filepath.Dir(dir); d != in.o.StateDir && within(d, in.o.StateDir); d = filepath.Dir(d) {
		out = append([]string{d}, out...)
	}
	return out
}

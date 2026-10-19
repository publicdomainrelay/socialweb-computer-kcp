package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
	"strings"
)

type InstalledRoll struct {
	Kcp           string
	Kine          string
	PlannedOutage bool
	Retention     kcpstore.BackupRetention
	Compat        []versions.KcpCompat
	ReadyTimeout  time.Duration
	OnListen      func(name string, addr net.Addr)
	Log           *slog.Logger
}

type InstalledRollReport struct {
	Pair      kcpserver.PairVerdict `json:"pair"`
	KcpFrom   kcpserver.Asset       `json:"kcpFrom"`
	KcpTo     kcpserver.Asset       `json:"kcpTo"`
	KineFrom  string                `json:"kineFrom"`
	KineTo    string                `json:"kineTo"`
	Roll      kcpserver.RollReport  `json:"roll"`
	Slots     kcpserver.Slots       `json:"slots"`
	Boot      []string              `json:"boot,omitempty"`
	BootError string                `json:"bootError,omitempty"`
}

func resolveTarget(link, target string) (string, error) {
	cur, err := filepath.EvalSymlinks(link)
	if err != nil {
		return "", fmt.Errorf("bootstrap: the asset link %s: %w", link, err)
	}
	if target == "" {
		return cur, nil
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("bootstrap: asset %s must be an absolute path", target)
	}
	return filepath.EvalSymlinks(target)
}

func RollInstalled(ctx context.Context, m hostsvc.Manager, p Params, o InstalledRoll) (InstalledRollReport, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	var out InstalledRollReport
	slots, err := kcpserver.ReadSlots(p.SlotsFile())
	if err != nil {
		return out, fmt.Errorf("bootstrap: no %s; this installed kcp has not booted: %w", p.SlotsFile(), err)
	}
	from := slots.Active
	if out.KcpFrom, err = kcpserver.ReadAsset(ctx, p.KcpPath); err != nil {
		return out, err
	}
	out.KcpTo = out.KcpFrom
	if o.Kcp != "" {
		if out.KcpTo, err = kcpserver.ReadAsset(ctx, o.Kcp); err != nil {
			return out, err
		}
	}
	if out.KineFrom, err = resolveTarget(p.KinePath, ""); err != nil {
		return out, err
	}
	if out.KineTo, err = resolveTarget(p.KinePath, o.Kine); err != nil {
		return out, err
	}
	out.Pair, err = kcpserver.CheckPair(out.KcpFrom, out.KcpTo, o.Compat, o.PlannedOutage)
	if err != nil {
		return out, err
	}
	app := p.App
	kcpChanged := out.KcpTo.Path != out.KcpFrom.Path
	kineChanged := out.KineTo != out.KineFrom
	outage := out.Pair.Mode == kcpserver.ModePlannedOutage || slots.Shape != kcpserver.ShapeBlueGreen
	to := kcpserver.Other(from)
	if slots.Shape != kcpserver.ShapeBlueGreen {
		to = from
		out.Pair.Mode = kcpserver.ModePlannedOutage
		out.Pair.Reason += "; a single-slot kcp rolls as a short restart of its slot"
	}
	if kineChanged && !outage && app.KineOf(to) == app.KineOf(from) {
		return out, fmt.Errorf("bootstrap: slots %s and %s share kine %s; a kine change needs --planned-outage", from, to, app.KineOf(from))
	}
	a, err := kcpserver.ReadSharedAdmin(p.SharedDir())
	if err != nil {
		return out, err
	}
	frontURL := slots.Front.URL()
	admin, err := kcpserver.NewHTTPAdmin(a, frontURL)
	if err != nil {
		return out, err
	}
	for _, slot := range app.Slots {
		if err := kcpserver.WriteSlotFiles(p.SlotRoot(slot)); err != nil {
			return out, err
		}
	}
	names := app.Names()
	retargeted, aborted := false, false
	point := func(kcp, kine string) error {
		if err := retarget(p.KcpPath, kcp); err != nil {
			return err
		}
		return retarget(p.KinePath, kine)
	}
	timeout := o.ReadyTimeout
	if timeout <= 0 {
		timeout = DefaultReadyTimeout
	}
	start := func(ctx context.Context, slot string, spec hostsvc.UnitSpec) (hostsvc.UnitStatus, error) {
		if (kcpChanged || kineChanged) && !retargeted && !aborted {
			if err := point(out.KcpTo.Path, out.KineTo); err != nil {
				return hostsvc.UnitStatus{}, err
			}
			retargeted = true
			log.Info("asset links retargeted", "kcp", out.KcpTo.Path, "kine", out.KineTo)
		}
		ks := app.KineOf(slot)
		kine := names.Kine(ks)
		if ks != app.KineOf(from) || kineChanged {
			m.Stop(ctx, kine)
			m.ResetFailed(ctx, kine)
		}
		if err := os.MkdirAll(p.KineDir(ks), 0o700); err != nil {
			return hostsvc.UnitStatus{}, err
		}
		if _, err := hostsvc.StartInstalled(ctx, m, kine); err != nil {
			return hostsvc.UnitStatus{}, err
		}
		kctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := waitUnixSocket(kctx, p.KineSocket(ks)); err != nil {
			return hostsvc.UnitStatus{}, fmt.Errorf("bootstrap: %s: %w", kine, err)
		}
		f, err := hostsvc.RenderInstalled(spec, hostsvc.InstallOptions{EnvFile: p.EnvFile(spec.Name)})
		if err != nil {
			return hostsvc.UnitStatus{}, err
		}
		if err := writePrivate(f.EnvPath, f.Env); err != nil {
			return hostsvc.UnitStatus{}, err
		}
		return hostsvc.StartInstalled(ctx, m, spec.Name)
	}
	abort := func(context.Context) error {
		if aborted {
			return nil
		}
		aborted = true
		if !retargeted {
			return nil
		}
		log.Warn("roll aborted; the asset links point back at the previous assets", "kcp", out.KcpFrom.Path, "kine", out.KineFrom)
		return point(out.KcpFrom.Path, out.KineFrom)
	}
	retire := func(ctx context.Context, old, current string) error {
		if app.KineOf(old) == app.KineOf(current) {
			return nil
		}
		kine := names.Kine(app.KineOf(old))
		if err := m.Stop(ctx, kine); err != nil {
			return err
		}
		m.ResetFailed(ctx, kine)
		return nil
	}
	rep, after, rerr := kcpserver.Roll(ctx, m, apiserverfront.Client{Path: p.FrontControl()}, admin, kcpserver.RollConfig{
		SlotsFile: p.SlotsFile(), Shared: p.SharedDir(), MemberToken: kcpserver.MemberTokenPath(p.Dir),
		SpecFor: func(slot string) (hostsvc.UnitSpec, string, error) {
			return p.slotSpec(slot, frontURL)
		},
		KineFor:  func(slot string) string { return p.KineURL(app.KineOf(slot)) },
		Backup:   p.storeBackup(o.Retention, log),
		Start:    start,
		Retire:   retire,
		Abort:    abort,
		Describe: func(string) (string, string) { return out.KcpTo.SHA256, out.KcpTo.Version },
		Outage:   outage, ReadyTimeout: o.ReadyTimeout, OnListen: o.OnListen, Log: log,
	})
	out.Roll, out.Slots = rep, after
	if rerr != nil && after.Active != rep.To {
		if err := abort(ctx); err != nil {
			rerr = errors.Join(rerr, err)
		}
	}
	if after.Active == rep.To && rep.To != rep.From {
		enable := []string{names.Slot(rep.To), names.Kine(app.KineOf(rep.To))}
		disable := []string{names.Slot(rep.From)}
		if app.KineOf(rep.From) != app.KineOf(rep.To) {
			disable = append(disable, names.Kine(app.KineOf(rep.From)))
		}
		if err := hostsvc.SetBoot(ctx, m, enable, disable); err != nil {
			out.BootError = err.Error()
			if !errors.Is(err, hostsvc.ErrUnsupported) {
				rerr = errors.Join(rerr, err)
			}
		} else {
			out.Boot = enable
		}
		pruneInactiveAuditLogs(p.SlotRoot(rep.From), log)
	}
	return out, rerr
}

// pruneInactiveAuditLogs removes the audit logs of a slot that has just been
// retired. They are the largest files a slot holds (100 MiB each, rotated), and
// an upgrade that is rolled back and forth otherwise leaves every generation's
// logs on disk.
func pruneInactiveAuditLogs(root string, log *slog.Logger) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "audit-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if err := os.Remove(filepath.Join(root, e.Name())); err == nil {
			removed++
		}
	}
	if removed > 0 {
		log.Info("removed the audit logs of the retired kcp slot", "slotRoot", root, "files", removed)
	}
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

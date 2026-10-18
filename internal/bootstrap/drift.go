package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

type SlotDrift struct {
	Slot        string   `json:"slot"`
	Unit        string   `json:"unit"`
	PID         int      `json:"pid"`
	Running     []string `json:"running,omitempty"`
	Want        []string `json:"want,omitempty"`
	RunningExe  string   `json:"runningExe,omitempty"`
	WantExe     string   `json:"wantExe,omitempty"`
	Refused     string   `json:"refused,omitempty"`
	ArgvChanged bool     `json:"argvChanged"`
	ExeChanged  bool     `json:"exeChanged"`
}

func (d SlotDrift) Drifted() bool { return d.ArgvChanged || d.ExeChanged }

func ActiveSlotDrift(ctx context.Context, m hostsvc.Manager, p Params) (SlotDrift, bool, error) {
	slots, err := kcpserver.ReadSlots(p.SlotsFile())
	if errors.Is(err, os.ErrNotExist) {
		return SlotDrift{}, false, nil
	}
	if err != nil {
		return SlotDrift{}, false, err
	}
	d := SlotDrift{Slot: slots.Active, Unit: p.App.Names().Slot(slots.Active)}
	st, err := m.Inspect(ctx, d.Unit)
	if err != nil || st.State != hostsvc.Active || st.MainPID <= 0 {
		return d, false, err
	}
	d.PID = st.MainPID
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(d.PID), "cmdline"))
	if err != nil {
		return d, false, err
	}
	d.Running = strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
	spec, _, err := p.slotSpec(slots.Active, slots.Front.URL())
	if err != nil {
		return d, false, err
	}
	d.Want = spec.Exec
	d.ArgvChanged = !slices.Equal(d.Running, d.Want)
	if err := kcpserver.CheckArgv(d.Running, slots.Shape); err != nil {
		d.Refused = err.Error()
	}
	d.RunningExe, _ = os.Readlink(filepath.Join("/proc", strconv.Itoa(d.PID), "exe"))
	d.WantExe, _ = filepath.EvalSymlinks(p.KcpPath)
	d.ExeChanged = d.RunningExe != "" && d.WantExe != "" && strings.TrimSuffix(d.RunningExe, " (deleted)") != d.WantExe
	return d, true, nil
}

func RollDrifted(ctx context.Context, m hostsvc.Manager, p Params, o InstalledRoll) (SlotDrift, *InstalledRollReport, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	d, ok, err := ActiveSlotDrift(ctx, m, p)
	if err != nil || !ok || !d.Drifted() {
		return d, nil, err
	}
	if d.ExeChanged {
		from, err := kcpserver.ReadAsset(ctx, strings.TrimSuffix(d.RunningExe, " (deleted)"))
		if err != nil {
			return d, nil, fmt.Errorf("bootstrap: the running kcp %s of %s: %w", d.RunningExe, d.Unit, err)
		}
		to, err := kcpserver.ReadAsset(ctx, p.KcpPath)
		if err != nil {
			return d, nil, err
		}
		if _, err := kcpserver.CheckPair(from, to, o.Compat, o.PlannedOutage); err != nil {
			return d, nil, fmt.Errorf("bootstrap: %s runs %s and the linked kcp is %s; not rolled: %w", d.Unit, from.Version, to.Version, err)
		}
	}
	lf, err := os.OpenFile(p.RollLock(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return d, nil, err
	}
	defer lf.Close()
	if err := hostos.Lock(lf, hostos.LockExclusive|hostos.LockNonBlocking); err != nil {
		return d, nil, fmt.Errorf("bootstrap: %s is held by another roll; %s keeps its old argv until that roll ends: %w", p.RollLock(), d.Unit, err)
	}
	log.Warn("the running kcp slot was started with another argv or kcp than this binary renders; rolling it",
		"unit", d.Unit, "pid", d.PID, "argvChanged", d.ArgvChanged, "exeChanged", d.ExeChanged, "refused", d.Refused,
		"running", strings.Join(d.Running, " "), "want", strings.Join(d.Want, " "))
	rep, err := RollInstalled(ctx, m, p, o)
	return d, &rep, err
}

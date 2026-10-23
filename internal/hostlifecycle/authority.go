package hostlifecycle

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

var ErrNoStateDir = errors.New("hostlifecycle: no host state directory is configured")

var ErrAccount = errors.New("hostlifecycle: the host account names no valid install record")

type KcpInstallationObservation struct {
	ActiveSlot   v1alpha1.KcpSlot
	FrontBackend string
	Drifted      bool
	DriftMessage string
	LeaseStale   bool
	LeaseMessage string
}

type HostInstallObservation struct {
	Found            bool
	Account          string
	InstalledVersion string
	Steps            []v1alpha1.HostInstallStep
}

type HostReleaseObservation struct {
	Current   string
	Previous  []string
	SelfCheck *v1alpha1.HostReleaseSelfCheck
	GC        *v1alpha1.HostReleaseGC
}

type Authority interface {
	ObserveKcpInstallation(ctx context.Context, spec v1alpha1.KcpInstallationSpec) (KcpInstallationObservation, error)
	ObserveHostInstall(ctx context.Context, spec v1alpha1.HostInstallSpec) (HostInstallObservation, error)
	ObserveHostRelease(ctx context.Context, spec v1alpha1.HostReleaseSpec) (HostReleaseObservation, error)
}

type FileAuthorityOptions struct {
	StateRoot    string
	StateDir     string
	Account      string
	SlotsFile    string
	FrontControl string
	Drift        func(context.Context) (bootstrap.SlotDrift, bool, error)
	Lease        func(context.Context) (kcpserver.Lease, error)
	SelfCheck    func(context.Context, string) *v1alpha1.HostReleaseSelfCheck
	GC           func(context.Context) *v1alpha1.HostReleaseGC
	Now          func() time.Time
}

type FileAuthority struct {
	o FileAuthorityOptions
}

func NewFileAuthority(o FileAuthorityOptions) *FileAuthority {
	return &FileAuthority{o: o}
}

func (a *FileAuthority) now() time.Time {
	if a.o.Now != nil {
		return a.o.Now()
	}
	return time.Now()
}

func (a *FileAuthority) ObserveKcpInstallation(ctx context.Context, _ v1alpha1.KcpInstallationSpec) (KcpInstallationObservation, error) {
	var out KcpInstallationObservation
	slots, err := kcpserver.ReadSlots(a.o.SlotsFile)
	if err != nil {
		return out, err
	}
	out.ActiveSlot = v1alpha1.KcpSlot(slots.Active)
	if a.o.FrontControl != "" {
		st, err := (apiserverfront.Client{Path: a.o.FrontControl}).Status(ctx)
		if err != nil {
			return out, err
		}
		out.FrontBackend = st.Backend
	}
	if a.o.Drift != nil {
		d, ok, err := a.o.Drift(ctx)
		if err != nil {
			return out, err
		}
		if ok && d.Drifted() {
			out.Drifted = true
			out.DriftMessage = "the active slot process differs from its unit"
		}
	}
	if a.o.Lease != nil {
		l, err := a.o.Lease(ctx)
		if err != nil {
			return out, err
		}
		if l.Found && !l.RenewTime.IsZero() && a.now().Sub(l.RenewTime) > bootstrap.LeaseStaleAfter {
			out.LeaseStale = true
			out.LeaseMessage = "the controller lease has not renewed for over " + bootstrap.LeaseStaleAfter.String()
		}
	}
	return out, nil
}

func (a *FileAuthority) ObserveHostInstall(ctx context.Context, spec v1alpha1.HostInstallSpec) (HostInstallObservation, error) {
	var out HostInstallObservation
	account := spec.Account
	account = cmp.Or(account, a.o.Account)
	if !validAccount(account) {
		return out, ErrAccount
	}
	out.Account = account
	rec, err := install.ReadRecord(a.o.StateRoot, account)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Found = true
	names := make([]string, 0, len(rec.Steps))
	for name := range rec.Steps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out.Steps = append(out.Steps, v1alpha1.HostInstallStep{Name: name, Phase: rec.Steps[name]})
	}
	stateDir := rec.StateDir
	stateDir = cmp.Or(stateDir, a.o.StateDir)
	if stateDir != "" {
		if asset, err := kcpserver.ReadAsset(ctx, filepath.Join(stateDir, "bin", "current", install.CoreName)); err == nil {
			out.InstalledVersion = versionOf(asset)
		}
	}
	return out, nil
}

func (a *FileAuthority) ObserveHostRelease(ctx context.Context, _ v1alpha1.HostReleaseSpec) (HostReleaseObservation, error) {
	var out HostReleaseObservation
	if a.o.StateDir == "" {
		return out, ErrNoStateDir
	}
	cur, err := kcpserver.ReadAsset(ctx, filepath.Join(a.o.StateDir, "bin", "current", install.CoreName))
	if err != nil {
		return out, err
	}
	out.Current = versionOf(cur)
	if prev, err := kcpserver.ReadAsset(ctx, filepath.Join(a.o.StateDir, "bin", "previous", install.CoreName)); err == nil {
		if v := versionOf(prev); v != "" && v != out.Current {
			out.Previous = []string{v}
		}
	}
	if a.o.SelfCheck != nil {
		out.SelfCheck = a.o.SelfCheck(ctx, out.Current)
	}
	if a.o.GC != nil {
		out.GC = a.o.GC(ctx)
	}
	return out, nil
}

func versionOf(asset kcpserver.Asset) string {
	if asset.Version != "" {
		return asset.Version
	}
	return asset.SHA256
}

func validAccount(account string) bool {
	return account != "" && account != "." && account != ".." && !strings.ContainsAny(account, `/\`)
}

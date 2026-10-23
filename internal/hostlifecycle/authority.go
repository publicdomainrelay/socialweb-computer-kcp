package hostlifecycle

import (
	"cmp"
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
)

const (
	LinkCurrent = "current"

	LinkPrevious = "previous"
)

var ErrNoStateDir = errors.New("hostlifecycle: no host state directory is configured")

var ErrAccount = errors.New("hostlifecycle: the host account names no valid install record")

var ErrNoReader = errors.New("hostlifecycle: the host reader is not configured")

type KcpInstallationObservation struct {
	ActiveSlot   v1alpha1.KcpSlot
	FrontBackend string
	Drifted      bool
	DriftMessage string
	LeaseStale   bool
	LeaseMessage string
	LeaseStaleIn time.Duration
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

type InstallRecord struct {
	StateDir string
	Steps    map[string]string
}

type FileAuthorityOptions struct {
	StateDir        string
	Account         string
	FrontControl    string
	Record          func(account string) (InstallRecord, error)
	ActiveSlot      func(context.Context) (string, error)
	CoreVersion     func(ctx context.Context, stateDir, link string) (string, error)
	Drift           func(context.Context) (bool, error)
	Lease           func(context.Context) (time.Time, bool, error)
	LeaseStaleAfter time.Duration
	SelfCheck       func(context.Context, string) *v1alpha1.HostReleaseSelfCheck
	GC              func(context.Context) *v1alpha1.HostReleaseGC
	Now             func() time.Time
}

type FileAuthority struct {
	o FileAuthorityOptions
}

var _ Authority = (*FileAuthority)(nil)

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
	if a.o.ActiveSlot == nil {
		return out, ErrNoReader
	}
	active, err := a.o.ActiveSlot(ctx)
	if err != nil {
		return out, err
	}
	out.ActiveSlot = v1alpha1.KcpSlot(active)
	if a.o.FrontControl != "" {
		st, err := (apiserverfront.Client{Path: a.o.FrontControl}).Status(ctx)
		if err != nil {
			return out, err
		}
		out.FrontBackend = st.Backend
	}
	if a.o.Drift != nil {
		drifted, err := a.o.Drift(ctx)
		if err != nil {
			return out, err
		}
		if drifted {
			out.Drifted = true
			out.DriftMessage = "the active slot process differs from its unit"
		}
	}
	if a.o.Lease != nil && a.o.LeaseStaleAfter > 0 {
		renewed, found, err := a.o.Lease(ctx)
		if err != nil {
			return out, err
		}
		if found && !renewed.IsZero() {
			left := a.o.LeaseStaleAfter - a.now().Sub(renewed)
			if left < 0 {
				out.LeaseStale = true
				out.LeaseMessage = "the controller lease has not renewed for over " + a.o.LeaseStaleAfter.String()
			} else {
				out.LeaseStaleIn = left + time.Millisecond
			}
		}
	}
	return out, nil
}

func (a *FileAuthority) ObserveHostInstall(ctx context.Context, spec v1alpha1.HostInstallSpec) (HostInstallObservation, error) {
	var out HostInstallObservation
	account := cmp.Or(spec.Account, a.o.Account)
	if !validAccount(account) {
		return out, ErrAccount
	}
	out.Account = account
	if a.o.Record == nil {
		return out, ErrNoReader
	}
	rec, err := a.o.Record(account)
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
	stateDir := cmp.Or(rec.StateDir, a.o.StateDir)
	if stateDir != "" && a.o.CoreVersion != nil {
		if v, err := a.o.CoreVersion(ctx, stateDir, LinkCurrent); err == nil {
			out.InstalledVersion = v
		}
	}
	return out, nil
}

func (a *FileAuthority) ObserveHostRelease(ctx context.Context, _ v1alpha1.HostReleaseSpec) (HostReleaseObservation, error) {
	var out HostReleaseObservation
	if a.o.StateDir == "" {
		return out, ErrNoStateDir
	}
	if a.o.CoreVersion == nil {
		return out, ErrNoReader
	}
	cur, err := a.o.CoreVersion(ctx, a.o.StateDir, LinkCurrent)
	if err != nil {
		return out, err
	}
	out.Current = cur
	if prev, err := a.o.CoreVersion(ctx, a.o.StateDir, LinkPrevious); err == nil && prev != "" && prev != out.Current {
		out.Previous = []string{prev}
	}
	if a.o.SelfCheck != nil {
		out.SelfCheck = a.o.SelfCheck(ctx, out.Current)
	}
	if a.o.GC != nil {
		out.GC = a.o.GC(ctx)
	}
	return out, nil
}

func validAccount(account string) bool {
	return account != "" && account != "." && account != ".." && !strings.ContainsAny(account, `/\`)
}

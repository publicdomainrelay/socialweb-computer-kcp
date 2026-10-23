package computevm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statesync"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

const (
	ChildPrefix    = "vm-"
	LabelComputeVM = "socialweb.computer/computevm"
	BidderSet      = "bidder"
	RetryAfter     = 3 * time.Second
	OrphanGrace    = 2 * time.Minute
	MessageCap     = 4000
	ApplyTimeout   = time.Minute
)

var GVR = v1alpha1.ComputeVMGVR

type Config struct {
	Kubeconfig  string
	Workspace   string
	Handle      string
	Runtime     vmruntime.Runtime
	Manager     hostsvc.Manager
	OrphanGrace time.Duration
	VaultServer string
	VaultClient string
	VaultMount  string
	Log         *slog.Logger
}

type Host struct {
	c Config

	Rest *rest.Config

	VMs dynamic.ResourceInterface

	Backends dynamic.ResourceInterface

	HostProcesses dynamic.ResourceInterface

	servers dynamic.ResourceInterface

	grants dynamic.ResourceInterface

	syncs dynamic.ResourceInterface
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "computevm", "handle", c.Handle)
}

func (c Config) Grace() time.Duration {
	return cmp.Or(c.OrphanGrace, OrphanGrace)
}

func ChildName(name string) string { return ChildPrefix + name }

func Open(ctx context.Context, c Config) (*Host, error) {
	if c.Handle == "" {
		return nil, errors.New("computevm: the controller names no handle; VM units and work dirs are namespaced by it")
	}
	if c.Runtime == nil || c.Manager == nil {
		return nil, errors.New("computevm: no VM runtime")
	}
	c.Workspace = cmp.Or(c.Workspace, hostprocess.DefaultWorkspace)
	restCfg, err := kcpserver.FrontRestConfig(c.Kubeconfig)
	if err != nil {
		return nil, err
	}
	if err := applySets(ctx, c, restCfg); err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(restCfg)
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + c.Workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	h := &Host{c: c, Rest: restCfg, VMs: dc.Resource(GVR), Backends: dc.Resource(v1alpha1.FirecrackerVMGVR),
		HostProcesses: dc.Resource(hostprocess.GVR), servers: dc.Resource(openbaoctl.GVR), grants: dc.Resource(openbaoctl.GrantGVR),
		syncs: dc.Resource(statesync.StateSyncGVR)}
	return h, nil
}

func (h *Host) Config() Config { return h.c }

func (h *Host) Log() *slog.Logger { return h.c.log() }

func applySets(ctx context.Context, c Config, restCfg *rest.Config) error {
	ap, err := kcpapply.New(restCfg, kcpapply.Options{WaitTimeout: ApplyTimeout, Base: c.Workspace, Log: c.log()})
	if err != nil {
		return err
	}
	ix, err := manifests.Load()
	if err != nil {
		return err
	}
	for _, name := range []string{hostprocess.ProviderSet, hostprocess.ManifestSet, BidderSet} {
		set, ok := ix.Set(name)
		if !ok {
			return fmt.Errorf("computevm: deploy/sets.yaml names no %s set", name)
		}
		values, err := manifests.DecodeValues(set.Values, nil)
		if err != nil {
			return err
		}
		if _, err := ap.Apply(ctx, set, values); err != nil {
			return fmt.Errorf("computevm: applying the %s set: %w", name, err)
		}
	}
	return nil
}

func CapMessage(s string) string {
	if len(s) <= MessageCap {
		return s
	}
	return s[:MessageCap/2] + "\n...\n" + s[len(s)-MessageCap/2:]
}

func Supervisor(c Config) hostprocess.Config {
	return hostprocess.Config{Kubeconfig: c.Kubeconfig, Workspace: cmp.Or(c.Workspace, hostprocess.DefaultWorkspace), Manager: c.Manager,
		Prefix: vmruntime.UnitPrefix(c.Handle), Bound: true, Selector: LabelComputeVM, KeepOrphans: true, Name: "computevm", Log: c.Log}
}

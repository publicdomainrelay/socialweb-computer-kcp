package computevm

import (
	"cmp"
	"context"
	"fmt"
	"sync"
	"time"

	kruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/compute/firecracker"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/firecrackervm"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const RestartMax = time.Minute

func Run(ctx context.Context, c computevm.Config) error {
	log := c.Log
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { hostprocess.Supervise(ctx, computevm.Supervisor(c)) })
	supervise.Restart(ctx, "computevm", supervise.Backoff{Min: computevm.RetryAfter, Max: RestartMax, OnError: func(err error, after time.Duration) {
		if log != nil {
			log.Error("the ComputeVM and FirecrackerVM controllers stopped; starting them again", "err", fmt.Sprint(err), "after", after.String())
		}
	}}, func(ctx context.Context) error { return run(ctx, c) })
	return nil
}

func run(ctx context.Context, c computevm.Config) error {
	host, err := computevm.Open(ctx, c)
	if err != nil {
		return err
	}
	workspace := cmp.Or(c.Workspace, hostprocess.DefaultWorkspace)
	mgr, err := ctlruntime.NewManager(ctlruntime.Config{Rest: host.Rest, ExportWorkspace: manifests.Rebase(workspace, ctlruntime.DefaultExportWorkspace),
		AddToScheme: []func(*kruntime.Scheme) error{v1alpha1.AddToScheme}, Log: c.Log})
	if err != nil {
		return err
	}
	if _, err := Register(mgr, host); err != nil {
		return err
	}
	placement := firecrackervm.NewPlacement(host)
	provider, err := firecracker.New(firecracker.Config{Handle: c.Handle, Log: c.Log},
		firecracker.Deps{Runtime: c.Runtime, Units: c.Manager, UserData: host, Placement: placement})
	if err != nil {
		return err
	}
	fc, err := firecrackervm.Register(mgr, provider, host, placement)
	if err != nil {
		return err
	}
	caps := c.Runtime.Capabilities()
	host.Log().Info("the ComputeVM and FirecrackerVM controllers run in the core", "workspace", workspace, "backend", string(caps.Backend),
		"hypervisor", caps.Hypervisor, "enforcesLimits", caps.EnforcesLimits, "gracefulStop", caps.GracefulStop)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		fc.Run(ctx)
		cancel()
	})
	return mgr.Start(ctx)
}

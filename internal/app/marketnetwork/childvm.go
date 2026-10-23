package marketnetwork

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/versions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

type BidderKcp struct {
	Server    string
	Workspace string
}

func KcpHandle(app v1alpha1.NetworkApp) string {
	if app.Kcp != nil && app.Kcp.Handle != "" {
		return strings.ReplaceAll(app.Kcp.Handle, "{app}", app.Name)
	}
	return app.Name + "." + BidderAccountDomain
}

func ChildVM(app v1alpha1.NetworkApp, l devinstance.Layout, parent devinstance.Record) (*devinstance.VMRuntime, error) {
	if app.Kind != v1alpha1.ApplicationKindMarketBidder || app.Kcp == nil {
		return nil, nil
	}
	k := app.Kcp
	backend := hostsvc.BackendProc
	if k.VmHostBackend != "" {
		b, err := hostsvc.ParseBackend(k.VmHostBackend)
		if err != nil {
			return nil, fmt.Errorf("apps[%s].kcp.vmHostBackend: %w", app.Name, err)
		}
		backend = b
	}
	if k.GuestImageSource == "" {
		return nil, fmt.Errorf("apps[%s].kcp names no guestImageSource; the ComputeVM controller fetches the pinned guest image from it", app.Name)
	}
	d, err := versions.VMStopGrace()
	if err != nil {
		return nil, err
	}
	grace := d.String()
	if k.StopGraceSeconds > 0 {
		grace = strconv.Itoa(int(k.StopGraceSeconds)) + "s"
	}
	vm := &devinstance.VMRuntime{Handle: KcpHandle(app), Backend: backend, ImageSource: k.GuestImageSource, StopGrace: grace,
		RuntimeDir: l.RuntimeDir, Executables: map[string]string{}}
	var missing []string
	for _, n := range vmruntime.ExecutableNames {
		e := parent.Executables[n]
		if e.Path == "" {
			missing = append(missing, n)
			continue
		}
		vm.Executables[n] = e.Path
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("apps[%s].kcp: the network instance records no %s; dev up takes the VM runtime's paths (userns-stage-path, pasta-path, vmexec-path, firecracker-path in a dev build)",
			app.Name, strings.Join(missing, ", "))
	}
	return vm, nil
}

func BidderKcpOf(rec devinstance.Record) BidderKcp {
	host, _, _ := strings.Cut(strings.TrimSuffix(rec.KcpURL, "/"), "/clusters/")
	return BidderKcp{Server: host, Workspace: cmp.Or(rec.BaseWorkspace, compose.DefaultWorkspace)}
}

package appctl

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
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
	ws := rec.BaseWorkspace
	if ws == "" {
		ws = DefaultWorkspace
	}
	return BidderKcp{Server: host, Workspace: ws}
}

var ErrFirecrackerInPod = errors.New("appctl: spec.firecracker: the bidder component runs no VM runtime command, so the firecracker provider cannot run in it; " +
	"use spec.kcp (the ComputeVM controller of the bidder's own core boots its guests)")

func providerValues(v *manifests.MarketBidder, spec v1alpha1.MarketBidderSpec, kcp BidderKcp) error {
	if spec.Firecracker != nil {
		return ErrFirecrackerInPod
	}
	k := spec.Kcp
	if k == nil {
		return errors.New("appctl: the MarketBidder names no provider; spec.kcp is the bidder's provider")
	}
	if kcp.Server == "" {
		return errors.New("appctl: spec.kcp: this instance records no kcp URL for the bidder's compute-provider-kcp")
	}
	handle := k.Handle
	if handle == "" {
		handle = spec.Identity.Handle
	}
	args, err := fragment([]string{"--compute-provider-kcp", "--compute-provider-kcp-server", kcp.Server,
		"--compute-provider-kcp-workspace", kcp.Workspace, "--compute-provider-kcp-handle", handle})
	if err != nil {
		return err
	}
	v.ProviderArgs = args
	v.Kcp = manifests.KcpRef{Server: kcp.Server, Workspace: kcp.Workspace}
	v.Guest.Handle = handle
	return nil
}

func (ctl *Controller) guestComponents(ctx context.Context) []v1alpha1.ApplicationComponent {
	var out []v1alpha1.ApplicationComponent
	for _, k := range []struct {
		kind     string
		gvr      schema.GroupVersionResource
		selector string
	}{{"ComputeVM", computevm.GVR, ""}, {"HostProcess", HostProcessGVR, computevm.LabelComputeVM}} {
		ri, err := ctl.client(ctl.c.workspace(), k.gvr)
		if err != nil {
			continue
		}
		list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: k.selector})
		if err != nil {
			continue
		}
		for _, o := range list.Items {
			phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
			out = append(out, v1alpha1.ApplicationComponent{Kind: k.kind, Name: o.GetName(), Workspace: ctl.c.workspace(), Phase: phase})
		}
	}
	return out
}

func (ctl *Controller) deleteComputeVMs(ctx context.Context) (int, error) {
	ri, err := ctl.client(ctl.c.workspace(), computevm.GVR)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range list.Items {
		if o.GetDeletionTimestamp() == nil {
			if err := ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(list.Items), err
			}
		}
	}
	return len(list.Items), nil
}

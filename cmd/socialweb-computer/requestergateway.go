package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	appgateway "github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketnetwork"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/gatewaykcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
)

var requesterGatewayOptions = []string{"name", "state-dir", "ready-file", "cluster", "network-timeout", "log-level"}

var requesterGatewayAssets = []string{gatewaykcp.ExeK3s, gatewaykcp.ExeExternalSecrets, gatewaykcp.ExeESOBundle, gatewaykcp.ExeWebDownloads,
	gatewaykcp.ExeDumbpipe, gatewaykcp.ExeKine, gatewaykcp.ExeBao}

func (r *requesterRun) gatewayPreflight(vals clicfg.Values) (map[string]devinstance.Executable, error) {
	results := hostcontract.EvaluateRequesterGateway(hostcontract.GatherRequesterGateway(hostcontract.Options{StateDir: r.dir}))
	if failed := hostcontract.Failed(results); len(failed) > 0 {
		hostcontract.Write(r.a.stderr, results, false)
		return nil, fmt.Errorf("--with-gateway runs k3s and kine in this account's user manager, and the host does not provide what they need: %s (check-host -role gateway names the same fixes)",
			strings.Join(failed, ", "))
	}
	if !assets.Release() {
		return nil, errors.New("--with-gateway runs the gateway's embedded k3s and external-secrets: build with scripts/build-socialweb-computer.sh (--dev for a dev build)")
	}
	cache, err := r.a.assetCache(vals)
	if err != nil {
		return nil, err
	}
	out := map[string]devinstance.Executable{}
	for _, name := range requesterGatewayAssets {
		e, err := assets.Resolve(assets.Options{Cache: cache, Log: r.a.log}, name)
		if err != nil {
			return nil, fmt.Errorf("--with-gateway: %w", err)
		}
		out[name] = devinstance.Executable{Path: e.Path, Version: e.Version}
	}
	return out, nil
}

func (r *requesterRun) instanceRecord() (devinstance.Record, error) {
	var rec devinstance.Record
	if r.instance != nil {
		return *r.instance, nil
	}
	if r.inv.Mode != request.ModeNetwork {
		err := fsx.ReadJSON(r.standalone.Record, &rec)
		return rec, err
	}
	netLayout, _, err := r.networkLayout()
	if err != nil {
		return rec, err
	}
	return r.childRecord(netLayout, r.inv.Instance)
}

func (r *requesterRun) startGateway(ctx context.Context, assetExes map[string]devinstance.Executable, did string) (<-chan error, *request.ReadyGateway, error) {
	rec, err := r.instanceRecord()
	if err != nil {
		return nil, nil, fmt.Errorf("the requester's instance record: %w", err)
	}
	exes := map[string]devinstance.Executable{}
	maps.Copy(exes, assetExes)
	for name, e := range rec.Executables {
		if e.Path != "" {
			exes[name] = e
		}
	}
	if exes[gatewaykcp.ExeCore].Path == "" {
		exe, err := selfExe()
		if err != nil {
			return nil, nil, err
		}
		exes[gatewaykcp.ExeCore] = devinstance.Executable{Path: exe}
	}
	spec := gatewaykcp.RequesterSpec(v1alpha1.GatewaySpec{}, request.RootName, exes[gatewaykcp.ExeWebDownloads].Path)
	cfg := gatewaykcp.Config{Backend: rec.Backend, Executables: exes, Spec: spec, RootName: appgateway.GatewayRootName, Environ: os.Environ(),
		Exe: exes[gatewaykcp.ExeCore].Path, Stderr: r.a.stderr, Log: r.a.log}
	ws := cmp.Or(strings.TrimSpace(rec.BaseWorkspace), r.workspace, "root")
	applied := make(chan struct{})
	errc := make(chan error, 1)
	go func() { errc <- gatewaykcp.RunRequester(ctx, cfg, rec, did, func() { close(applied) }) }()
	select {
	case err := <-errc:
		return nil, nil, fmt.Errorf("the gateway did not start: %w", err)
	case <-applied:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	r.log("the Gateway root (profile requester) is applied; its clusterprovider runs in this process", "workspace", ws, "root", appgateway.GatewayRootName)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	waited := make(chan error, 1)
	go func() {
		_, err := gatewaykcp.WaitRequesterGateway(wctx, rec.AdminKubeconfig, ws, appgateway.GatewayRootName, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
			r.log("gateway", "phase", string(st.Phase), "message", st.Message)
		})
		waited <- err
	}()
	select {
	case err := <-errc:
		return nil, nil, fmt.Errorf("the gateway's clusterprovider ended: %w", err)
	case err := <-waited:
		if err != nil {
			return nil, nil, err
		}
	}
	return errc, &request.ReadyGateway{Workspace: ws, Name: appgateway.GatewayRootName, Clusters: appgateway.ClusterStateDir(rec.Layout.StateDir)}, nil
}

func runRequesterKubeconfig(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	ready, _, _, code := readRequesterReady(a, vals, rest, "kubeconfig")
	if code != exitOK {
		return code
	}
	cluster := strings.TrimSpace(vals.String("cluster"))
	if cluster == "" {
		return a.fail(exitRefused, "run requester kubeconfig: --cluster names the tenant cluster")
	}
	if ready.Gateway == nil {
		return a.fail(exitRefused, "run requester kubeconfig: the requester %s runs no gateway (start it with --with-gateway)", ready.Name)
	}
	path, err := gatewaykcp.TenantAdminKubeconfig(ready.Gateway.Clusters, ready.Did, cluster)
	if err != nil {
		return a.fail(exitRefused, "run requester kubeconfig: %v", err)
	}
	if !fsx.Exists(path) {
		return a.fail(exitFailed, "run requester kubeconfig: the cluster %s has no admin kubeconfig yet (%s); kubectl get clusters.socialweb.computer in its home workspace shows its phase", cluster, path)
	}
	fmt.Fprintln(a.stdout, path)
	return exitOK
}

func runRequesterCluster(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	ready, _, _, code := readRequesterReady(a, vals, rest, "cluster")
	if code != exitOK {
		return code
	}
	cluster := strings.TrimSpace(vals.String("cluster"))
	if cluster == "" {
		return a.fail(exitRefused, "run requester cluster: --cluster names the tenant cluster")
	}
	if ready.Gateway == nil {
		return a.fail(exitRefused, "run requester cluster: the requester %s runs no gateway (start it with --with-gateway)", ready.Name)
	}
	if ready.Did == "" {
		return a.fail(exitFailed, "run requester cluster: the ready file names no did yet")
	}
	timeout, err := time.ParseDuration(vals.String("network-timeout"))
	if err != nil {
		return a.fail(exitRefused, "run requester cluster: network-timeout: %v", err)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	home, err := gatewaykcp.RequesterCluster(cctx, ready.Kubeconfig, ready.Gateway.Workspace, ready.Did, cluster, a.log)
	if err != nil {
		return a.fail(exitFailed, "run requester cluster: %v", err)
	}
	a.printJSON(map[string]string{"cluster": cluster, "account": ready.Did, "home": home,
		"kubeconfig": "run requester kubeconfig " + ready.Name + " --cluster " + cluster})
	return exitOK
}

func requesterGatewayMarker(l devinstance.Layout) string {
	return filepath.Join(l.StateDir, gatewaykcp.RequesterGatewayDir)
}

func (a *app) requesterGatewayOutbound(ctx context.Context, rec devinstance.Record) (func(), error) {
	if rec.Gateway != nil || rec.SharedKcp == "" || rec.AdminKubeconfig == "" || !fsx.Exists(requesterGatewayMarker(rec.Layout)) {
		return func() {}, nil
	}
	if err := os.MkdirAll(rec.Layout.KcpDir(), 0o700); err != nil {
		return nil, err
	}
	n, err := marketnetwork.ReadNetwork(ctx, rec.AdminKubeconfig)
	if err != nil {
		return nil, err
	}
	_, stop, err := gatewaykcp.NetworkOutbound(ctx, n.Status.Network, filepath.Join(requesterGatewayMarker(rec.Layout), "tls"),
		gatewaykcp.RequesterEgressRecord(rec.Layout), a.log)
	return stop, err
}

func (r *requesterRun) markGateway(ctx context.Context) error {
	if r.inv.Mode != request.ModeNetwork {
		return nil
	}
	netLayout, netRec, err := r.networkLayout()
	if err != nil {
		return err
	}
	n, err := marketnetwork.ReadNetwork(ctx, netRec.AdminKubeconfig)
	if err != nil {
		return err
	}
	for _, app := range n.Spec.Apps {
		if app.Kind == v1alpha1.ApplicationKindGateway {
			return fmt.Errorf("--with-gateway: the market network %s has the gateway child %s, whose clusterprovider serves every Cluster of the network's kcp; bring the network up without --with-gateway",
				r.inv.Network, app.Name)
		}
	}
	cl, err := devinstance.NewLayout(netLayout.ID+"-"+r.inv.Name, filepath.Join(netLayout.StateDir, compose.AppsDir), filepath.Join(netLayout.RuntimeDir, compose.AppsDir))
	if err != nil {
		return err
	}
	return os.MkdirAll(requesterGatewayMarker(cl), 0o700)
}

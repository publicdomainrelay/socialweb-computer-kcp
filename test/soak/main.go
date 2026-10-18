//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
)

const rfpOnlyAnnotation = "socialweb.computer/rfp-only"

type target struct {
	instance, stateDir string
	layout             devinstance.Layout
	record             devinstance.Record
}

func openTarget(instance, stateDir string) (*target, error) {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	l, err := devinstance.NewLayout(instance, stateDir, rt)
	if err != nil {
		return nil, err
	}
	t := &target{instance: instance, stateDir: stateDir, layout: l}
	if err := devinstance.ReadJSON(l.Record, &t.record); err != nil {
		return nil, fmt.Errorf("instance %s: %w", instance, err)
	}
	return t, nil
}

func (t *target) childRecord(instance string) (devinstance.Record, error) {
	cl, err := devinstance.NewLayout(instance, filepath.Join(t.layout.StateDir, appctl.AppsDir), filepath.Join(t.layout.RuntimeDir, appctl.AppsDir))
	if err != nil {
		return devinstance.Record{}, err
	}
	var rec devinstance.Record
	return rec, devinstance.ReadJSON(cl.Record, &rec)
}

func workspaceOf(rec devinstance.Record) string {
	if rec.BaseWorkspace != "" {
		return rec.BaseWorkspace
	}
	return appctl.DefaultWorkspace
}

func clientOf(kubeconfig, ws string) (dynamic.Interface, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	c := rest.CopyConfig(cfg)
	c.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + ws
	c.Timeout = 30 * time.Second
	c.QPS, c.Burst = 200, 400
	return dynamic.NewForConfig(c)
}

type counters struct {
	rfpCreated, rfpFailed, vmStarted, vmHeld, vmReleased, vmFailed, refused atomic.Int64
	tenantCycles, tenantOut, tenantIn, tenantFailed                         atomic.Int64
	clustersCreated, clustersDeleted                                        atomic.Int64
}

func (c *counters) line(started time.Time) string {
	b, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "elapsedSec": int(time.Since(started).Seconds()),
		"rfpCreated": c.rfpCreated.Load(), "rfpFailed": c.rfpFailed.Load(), "vmStarted": c.vmStarted.Load(), "vmHeld": c.vmHeld.Load(),
		"vmReleased": c.vmReleased.Load(), "vmFailed": c.vmFailed.Load(), "refused": c.refused.Load(),
		"tenantCycles": c.tenantCycles.Load(), "tenantOut": c.tenantOut.Load(), "tenantIn": c.tenantIn.Load(), "tenantFailed": c.tenantFailed.Load(),
		"clustersCreated": c.clustersCreated.Load(), "clustersDeleted": c.clustersDeleted.Load()})
	return string(b)
}

func contractBody(name, ttl string, rfpOnly bool, spec v1alpha1.ComputeContractSpec) (*unstructured.Unstructured, error) {
	ann := map[string]string{v1alpha1.AnnotationTTLAfterFinished: ttl}
	if rfpOnly {
		ann[rfpOnlyAnnotation] = "true"
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}, Spec: spec})
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("ComputeContract")
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

func phaseOf(u *unstructured.Unstructured) string {
	p, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	return p
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func runLoad(args []string) int {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	name := fs.String("requester", "soakreq", "name of the long-lived MarketRequester child")
	rfpRate := fs.Float64("rfp-rate", 0.5, "RFP-only contracts per second")
	vmEvery := fs.Duration("vm-every", 45*time.Second, "interval between full contracts that boot a VM")
	vmLanes := fs.Int("vm-lanes", 3, "most full contracts in flight")
	vmHold := fs.Duration("vm-hold", 20*time.Second, "how long a full contract is held before its release")
	duration := fs.Duration("duration", 0, "stop after this long (0: until a signal)")
	ttl := fs.String("contract-ttl", "50m", "ttl-after-finished of every contract")
	retentionSec := fs.Int("record-retention-sec", 1800, "requester record retention")
	scanMs := fs.Int("record-retention-scan-ms", 60000, "requester record retention scan interval")
	maxConcurrent := fs.Int("max-concurrent", 64, "requester max concurrent contracts")
	contractQuota := fs.Int("contract-quota", 20000, "requester ComputeContract quota")
	inflight := fs.Int("rfp-inflight", 48, "most RFP-only contracts being created at once")
	tenantEvery := fs.Duration("tenant-every", 0, "interval between tenant cluster scale-out and scale-in cycles (0: off)")
	tenantName := fs.String("tenant-name", "tenant", "tenant cluster name")
	tenantLanes := fs.Int("tenant-lanes", 0, "parallel lanes that each create a tenant cluster, scale a node out and in, and delete the cluster")
	laneBase := fs.Int("lane-base", 0, "number added to the tenant lane numbers, so two processes never share a cluster name")
	tenantHold := fs.Duration("tenant-hold", 20*time.Second, "how long the tenant pod runs before it is deleted")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if _, found, err := request.NetworkApp(ctx, t.record.AdminKubeconfig, *name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	} else if !found {
		app := v1alpha1.NetworkApp{Name: *name, Kind: v1alpha1.ApplicationKindMarketRequester, Profile: appctl.RequesterProfileNetwork,
			Quota: &v1alpha1.ApplicationQuota{ComputeContracts: int32(*contractQuota)},
			Options: []v1alpha1.ApplicationOption{
				{Name: "record-retention-sec", Value: fmt.Sprint(*retentionSec)},
				{Name: "record-retention-scan-ms", Value: fmt.Sprint(*scanMs)},
				{Name: "max-concurrent", Value: fmt.Sprint(*maxConcurrent)},
				{Name: "per-account-max", Value: fmt.Sprint(*maxConcurrent)},
			}}
		if err := request.AddToNetwork(ctx, t.record.AdminKubeconfig, app); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	app, err := request.WaitNetworkApp(ctx, t.record.AdminKubeconfig, *name, 10*time.Minute, func(a v1alpha1.NetworkAppStatus) bool {
		return a.Instance != "" && a.KcpUrl != "" && a.Message != appctl.StoppedMessage && a.Phase == v1alpha1.ApplicationPhaseReady
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rec, err := t.childRecord(app.Instance)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dyn, err := clientOf(rec.AdminKubeconfig, workspaceOf(rec))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	contracts := dyn.Resource(appctl.ComputeContractGVR)
	for deadline := time.Now().Add(3 * time.Minute); ; {
		if _, err := contracts.List(ctx, metav1.ListOptions{Limit: 1}); err == nil || time.Now().After(deadline) {
			break
		}
		sleepCtx(ctx, 2*time.Second)
	}
	fmt.Println(`{"event":"requester ready","instance":"` + app.Instance + `"}`)

	started := time.Now()
	run := ctx
	if *duration > 0 {
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	var c counters
	var wg sync.WaitGroup
	epoch := time.Now().Unix()

	create := func(u *unstructured.Unstructured) bool {
		_, err := contracts.Create(ctx, u, metav1.CreateOptions{FieldManager: request.FieldManager})
		switch {
		case err == nil:
			return true
		case apierrors.IsForbidden(err) || apierrors.IsInvalid(err) && strings.Contains(err.Error(), "quota"):
			c.refused.Add(1)
		default:
			if c.rfpFailed.Add(1)%20 == 1 {
				fmt.Fprintln(os.Stderr, "create:", err)
			}
		}
		return false
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for tm := time.NewTicker(time.Minute); ; {
			select {
			case <-run.Done():
				return
			case <-tm.C:
				fmt.Println(c.line(started))
			}
		}
	}()

	if *rfpRate > 0 {
		sem := make(chan struct{}, *inflight)
		wg.Add(1)
		go func() {
			defer wg.Done()
			interval := time.Duration(float64(time.Second) / *rfpRate)
			next := time.Now()
			for seq := 0; ; seq++ {
				next = next.Add(interval)
				if !sleepCtx(run, time.Until(next)) {
					return
				}
				select {
				case sem <- struct{}{}:
				case <-run.Done():
					return
				}
				go func(seq int) {
					defer func() { <-sem }()
					u, err := contractBody(fmt.Sprintf("rfp-%d-%d", epoch, seq), *ttl, true, v1alpha1.ComputeContractSpec{VMName: "soak-rfp"})
					if err == nil && create(u) {
						c.rfpCreated.Add(1)
					}
				}(seq)
			}
		}()
	}

	if *vmEvery > 0 {
		lanes := make(chan struct{}, *vmLanes)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := 0; ; seq++ {
				if !sleepCtx(run, *vmEvery) {
					return
				}
				select {
				case lanes <- struct{}{}:
				case <-run.Done():
					return
				}
				wg.Add(1)
				go func(seq int) {
					defer wg.Done()
					defer func() { <-lanes }()
					name := fmt.Sprintf("vm-%d-%d", epoch, seq)
					u, err := contractBody(name, *ttl, false, v1alpha1.ComputeContractSpec{VMName: name})
					if err != nil || !create(u) {
						return
					}
					c.vmStarted.Add(1)
					held := waitPhase(ctx, contracts, name, 10*time.Minute, "Held", "Failed", "Released")
					if held != "Held" {
						c.vmFailed.Add(1)
						contracts.Patch(ctx, name, types.MergePatchType, []byte(`{"spec":{"release":true}}`), metav1.PatchOptions{})
						return
					}
					c.vmHeld.Add(1)
					sleepCtx(ctx, *vmHold)
					contracts.Patch(ctx, name, types.MergePatchType, []byte(`{"spec":{"release":true}}`), metav1.PatchOptions{})
					if waitPhase(ctx, contracts, name, 5*time.Minute, "Released", "Failed") == "Released" {
						c.vmReleased.Add(1)
					} else {
						c.vmFailed.Add(1)
					}
				}(seq)
			}
		}()
	}

	if *tenantEvery > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gateway := filepath.Join(t.layout.StateDir, appctl.AppsDir, "dev", *instance+"-gateway")
			for seq := 0; ; seq++ {
				if !sleepCtx(run, *tenantEvery) {
					return
				}
				kube, err := openTenantKube(gateway, *tenantName)
				if err != nil {
					c.tenantFailed.Add(1)
					fmt.Println(`{"event":"tenant open failed","error":"` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`)
					continue
				}
				c.tenantCycles.Add(1)
				out, in, err := tenantCycle(ctx, kube, seq, *tenantHold, 6*time.Minute, 5*time.Minute)
				if out {
					c.tenantOut.Add(1)
				}
				if in {
					c.tenantIn.Add(1)
				}
				if err != nil {
					c.tenantFailed.Add(1)
					fmt.Println(`{"event":"tenant cycle failed","error":"` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`)
				}
			}
		}()
	}

	if *tenantLanes > 0 {
		gateway := filepath.Join(t.layout.StateDir, appctl.AppsDir, "dev", *instance+"-gateway")
		grec, err := t.childRecord(*instance + "-gateway")
		if err != nil || grec.Gateway == nil {
			fmt.Fprintln(os.Stderr, "the gateway record:", err)
			return 1
		}
		for lane := *laneBase + 1; lane <= *laneBase+*tenantLanes; lane++ {
			wg.Add(1)
			go func(lane int) {
				defer wg.Done()
				if !sleepCtx(run, time.Duration(lane)*20*time.Second) {
					return
				}
				tenantLane(run, lane, gateway, grec.Gateway.BootstrapAccountDID, grec.BaseWorkspace, grec.AdminKubeconfig, &c, *tenantHold, func(m string) {
					fmt.Println(`{"event":"` + strings.ReplaceAll(m, `"`, `'`) + `"}`)
				})
			}(lane)
		}
	}

	<-run.Done()
	fmt.Println(c.line(started))
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Minute):
	}
	fmt.Println(c.line(started))
	return 0
}

func waitPhase(ctx context.Context, contracts dynamic.NamespaceableResourceInterface, name string, timeout time.Duration, want ...string) string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		u, err := contracts.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			p := phaseOf(u)
			for _, w := range want {
				if p == w {
					return p
				}
			}
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return ""
		}
	}
	return "timeout"
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: soak load|sample [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "load":
		os.Exit(runLoad(os.Args[2:]))
	case "sample":
		os.Exit(runSample(os.Args[2:]))
	case "hammer":
		os.Exit(runHammer(os.Args[2:]))
	case "tidy":
		os.Exit(runTidy(os.Args[2:]))
	case "inspect":
		os.Exit(runInspect(os.Args[2:]))
	case "orphans":
		os.Exit(runOrphans(os.Args[2:]))
	case "release":
		os.Exit(runRelease(os.Args[2:]))
	}
	fmt.Fprintln(os.Stderr, "usage: soak load|sample [flags]")
	os.Exit(2)
}

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const appStatusTimeout = 10 * time.Second

type rootStatusView struct {
	Kind    string                     `json:"kind"`
	Name    string                     `json:"name"`
	Phase   v1alpha1.ApplicationPhase  `json:"phase,omitempty"`
	Message string                     `json:"message,omitempty"`
	Market  *v1alpha1.MarketLoopStatus `json:"market,omitempty"`
	Store   *v1alpha1.StoreUsage       `json:"store,omitempty"`
	GC      *v1alpha1.GCStatus         `json:"gc,omitempty"`
	TTL     *v1alpha1.TTLStatus        `json:"ttl,omitempty"`
	Quotas  []v1alpha1.QuotaUsage      `json:"quotas,omitempty"`
}

type contractStatusView struct {
	Name      string                        `json:"name"`
	Phase     v1alpha1.ComputeContractPhase `json:"phase,omitempty"`
	Message   string                        `json:"message,omitempty"`
	Execution *v1alpha1.ExecutionStatus     `json:"execution,omitempty"`
	Winner    string                        `json:"winner,omitempty"`
}

type appStatusView struct {
	App       string               `json:"app"`
	Instance  string               `json:"instance"`
	Roots     []rootStatusView     `json:"roots,omitempty"`
	Contracts []contractStatusView `json:"contracts,omitempty"`
	Error     string               `json:"error,omitempty"`
}

type appKcp struct {
	app, instance, kubeconfig, workspace string
}

func runStatus(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) > 1 {
		return a.fail(exitRefused, "status: status [<app>] [--instance <instance>]")
	}
	want := ""
	if len(rest) == 1 {
		want = rest[0]
	}
	l, rec, code := a.record("status", vals)
	if code != exitOK {
		return code
	}
	apps, err := appKcps(ctx, l, rec)
	if err != nil {
		return a.fail(exitFailed, "status: %v", err)
	}
	var out []appStatusView
	var names []string
	for _, ak := range apps {
		names = append(names, ak.app)
		if want != "" && ak.app != want {
			continue
		}
		out = append(out, readAppStatus(ctx, ak))
	}
	if want != "" && len(out) == 0 {
		return a.fail(exitFailed, "status: instance %s has no application %s (it has: %s)", l.ID, want, strings.Join(names, ", "))
	}
	a.printJSON(out)
	for _, v := range out {
		if v.Error != "" {
			return exitFailed
		}
	}
	return exitOK
}

func appKcps(ctx context.Context, l devinstance.Layout, rec devinstance.Record) ([]appKcp, error) {
	if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
		return []appKcp{{app: l.ID, instance: l.ID, kubeconfig: rec.AdminKubeconfig, workspace: baseWorkspace(rec)}}, nil
	}
	n, err := appctl.ReadNetwork(ctx, rec.AdminKubeconfig)
	if err != nil {
		return nil, fmt.Errorf("reading the MarketNetwork of %s: %w", l.ID, err)
	}
	var out []appKcp
	for _, ap := range n.Status.Apps {
		if ap.Instance == "" {
			continue
		}
		cl, err := devinstance.NewLayout(ap.Instance, filepath.Join(l.StateDir, appctl.AppsDir), filepath.Join(l.RuntimeDir, appctl.AppsDir))
		if err != nil {
			return nil, err
		}
		var crec devinstance.Record
		if err := fsx.ReadJSON(cl.Record, &crec); err != nil {
			continue
		}
		out = append(out, appKcp{app: ap.Name, instance: ap.Instance, kubeconfig: crec.AdminKubeconfig, workspace: baseWorkspace(crec)})
	}
	return out, nil
}

func baseWorkspace(rec devinstance.Record) string {
	if rec.BaseWorkspace != "" {
		return rec.BaseWorkspace
	}
	return appctl.DefaultWorkspace
}

func readAppStatus(ctx context.Context, ak appKcp) appStatusView {
	v := appStatusView{App: ak.app, Instance: ak.instance}
	ctx, cancel := context.WithTimeout(ctx, appStatusTimeout)
	defer cancel()
	dc, err := workspaceClient(ak.kubeconfig, ak.workspace)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	for _, r := range []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{{appctl.MarketBidderGVR, "MarketBidder"}, {appctl.MarketRequesterGVR, "MarketRequester"}, {appctl.GatewayGVR, "Gateway"}} {
		list, err := dc.Resource(r.gvr).List(ctx, metav1.ListOptions{})
		if absentResource(err) {
			continue
		}
		if err != nil {
			v.Error = fmt.Sprintf("listing %s: %v", r.gvr.Resource, err)
			return v
		}
		for i := range list.Items {
			var o struct {
				metav1.ObjectMeta `json:"metadata"`
				Status            v1alpha1.ApplicationStatus `json:"status"`
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &o); err != nil {
				v.Error = err.Error()
				return v
			}
			v.Roots = append(v.Roots, rootStatusView{Kind: r.kind, Name: o.Name, Phase: o.Status.Phase, Message: o.Status.Message,
				Market: o.Status.Market, Store: o.Status.Store, GC: o.Status.GC, TTL: o.Status.TTL, Quotas: o.Status.Quotas})
		}
	}
	list, err := dc.Resource(appctl.ComputeContractGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil && !absentResource(err) {
		v.Error = fmt.Sprintf("listing computecontracts: %v", err)
		return v
	}
	if list != nil {
		for i := range list.Items {
			var c v1alpha1.ComputeContract
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &c); err != nil {
				v.Error = err.Error()
				return v
			}
			cv := contractStatusView{Name: c.Name, Phase: c.Status.Phase, Message: c.Status.Message, Execution: c.Status.Execution}
			if ct := c.Status.Contract; ct != nil && ct.Winner != nil {
				cv.Winner = ct.Winner.DID
			}
			v.Contracts = append(v.Contracts, cv)
		}
	}
	return v
}

func absentResource(err error) bool {
	return err != nil && (apierrors.IsNotFound(err) || meta.IsNoMatchError(err))
}

func workspaceClient(kubeconfig, ws string) (dynamic.Interface, error) {
	base, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg := rest.CopyConfig(base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + ws
	cfg.Timeout = appStatusTimeout
	return dynamic.NewForConfig(cfg)
}

func appStatuses(ctx context.Context, l devinstance.Layout, rec devinstance.Record) []appStatusView {
	apps, err := appKcps(ctx, l, rec)
	if err != nil {
		return []appStatusView{{App: l.ID, Instance: l.ID, Error: err.Error()}}
	}
	out := make([]appStatusView, 0, len(apps))
	for _, ak := range apps {
		out = append(out, readAppStatus(ctx, ak))
	}
	return out
}

package bidder

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/statuspatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

var (
	BidGVR = v1alpha1.BidGVR

	ComputeVMGVR = v1alpha1.ComputeVMGVR

	GuestIdentityGrantGVR = v1alpha1.GuestIdentityGrantGVR

	MarketIdentityGVR = v1alpha1.MarketIdentityGVR

	OpenBaoServerGVR = v1alpha1.OpenBaoServerGVR

	PolicyWorkflowRunGVR = policyv1alpha1.PolicyWorkflowRunGVR
)

func kcpStore(deps component.Deps, workspace string) (*kcpstore.Store, string, error) {
	if deps.KCP == nil {
		return nil, "", errors.New("bidder: the component has no kcp clients")
	}
	workspace = cmp.Or(workspace, "root")
	client, err := deps.KCP.Client(workspace)
	if err != nil {
		return nil, "", fmt.Errorf("bidder: kcp client for %s: %w", workspace, err)
	}
	cfg := client.RESTConfig()
	if cfg == nil || cfg.Host == "" {
		return nil, "", fmt.Errorf("bidder: the kcp client for %s has no rest config", workspace)
	}
	store, err := kcpstore.New(kcpstore.Options{Host: cfg.Host, RestConfig: cfg})
	if err != nil {
		return nil, "", err
	}
	return store, workspace, nil
}

func statusMap(status any) (map[string]any, error) {
	raw, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type KCPBids struct {
	Res *kcpstore.ClusterResource[v1alpha1.Bid]

	Cluster string

	Log *slog.Logger
}

func NewKCPBids(store *kcpstore.Store, cluster string) *KCPBids {
	return &KCPBids{Res: kcpstore.NewClusterResource[v1alpha1.Bid](store, BidGVR), Cluster: cluster}
}

func (b *KCPBids) Create(ctx context.Context, bid *v1alpha1.Bid) error {
	if b == nil || b.Res == nil {
		return errors.New("bidder: the Bid store is not configured")
	}
	bid.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Bid"}
	return b.Res.Create(ctx, b.Cluster, bid)
}

func (b *KCPBids) Get(ctx context.Context, name string) (*v1alpha1.Bid, error) {
	if b == nil || b.Res == nil {
		return nil, errors.New("bidder: the Bid store is not configured")
	}
	bid, err := b.Res.Get(ctx, b.Cluster, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return bid, err
}

func (b *KCPBids) List(ctx context.Context) ([]v1alpha1.Bid, error) {
	if b == nil || b.Res == nil {
		return nil, errors.New("bidder: the Bid store is not configured")
	}
	return b.Res.List(ctx, b.Cluster)
}

func (b *KCPBids) PatchSpec(ctx context.Context, name string, spec v1alpha1.BidSpec) error {
	if b == nil || b.Res == nil {
		return errors.New("bidder: the Bid store is not configured")
	}
	patch, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return err
	}
	return b.Res.PatchMerge(ctx, b.Cluster, name, patch)
}

func (b *KCPBids) PatchStatus(ctx context.Context, name string, status v1alpha1.BidStatus) error {
	if b == nil || b.Res == nil {
		return errors.New("bidder: the Bid store is not configured")
	}
	current, err := b.Res.Get(ctx, b.Cluster, name)
	if err != nil {
		return err
	}
	value, err := statusMap(status)
	if err != nil {
		return err
	}
	if status.LastError == "" {
		value["lastError"] = ""
	}
	patch, err := statuspatch.Merge(value)
	if err != nil {
		return err
	}
	return b.Res.PatchStatus(ctx, b.Cluster, name, current.ResourceVersion, patch)
}

type KCPVMs struct {
	Res *kcpstore.ClusterResource[v1alpha1.ComputeVM]

	Cluster string

	Handle string

	Log *slog.Logger
}

func NewKCPVMs(store *kcpstore.Store, cluster, handle string) *KCPVMs {
	return &KCPVMs{Res: kcpstore.NewClusterResource[v1alpha1.ComputeVM](store, ComputeVMGVR), Cluster: cluster, Handle: handle}
}

func (v *KCPVMs) log() *slog.Logger {
	if v.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return v.Log
}

func (v *KCPVMs) Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("bidder: the ComputeVM store is not configured")
	}
	vm, err := v.Res.Get(ctx, v.Cluster, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return vm, nil
}

func (v *KCPVMs) List(ctx context.Context) ([]v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("bidder: the ComputeVM store is not configured")
	}
	return v.Res.List(ctx, v.Cluster)
}

func (v *KCPVMs) Create(ctx context.Context, vm *v1alpha1.ComputeVM) error {
	if v == nil || v.Res == nil {
		return errors.New("bidder: the ComputeVM store is not configured")
	}
	vm.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM"}
	return v.Res.Create(ctx, v.Cluster, vm)
}

func (v *KCPVMs) Delete(ctx context.Context, name string) error {
	if v == nil || v.Res == nil {
		return errors.New("bidder: the ComputeVM store is not configured")
	}
	err := v.Res.Delete(ctx, v.Cluster, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (v *KCPVMs) PatchMarket(ctx context.Context, name string, patch v1alpha1.ComputeVMMarket) error {
	if v == nil || v.Res == nil {
		return errors.New("bidder: the ComputeVM store is not configured")
	}
	owned := map[string]any{}
	if patch.OnNetworkURI != "" {
		owned["onNetworkUri"] = patch.OnNetworkURI
	}
	if patch.HostKeysURI != "" {
		owned["hostKeysUri"] = patch.HostKeysURI
	}
	if patch.Termination != nil {
		termination := map[string]any{}
		if patch.Termination.Reason != "" {
			termination["reason"] = patch.Termination.Reason
		}
		if patch.Termination.ObservedAt != "" {
			termination["observedAt"] = patch.Termination.ObservedAt
		}
		if patch.Termination.ExitCode != nil {
			termination["exitCode"] = *patch.Termination.ExitCode
		}
		if patch.Termination.Request != nil {
			termination["request"] = map[string]any{"uri": patch.Termination.Request.URI, "cid": patch.Termination.Request.CID}
		}
		if patch.Termination.PayloadURI != "" {
			termination["payloadUri"] = patch.Termination.PayloadURI
		}
		if patch.Termination.PayloadCid != "" {
			termination["payloadCid"] = patch.Termination.PayloadCid
		}
		if patch.Termination.EventURI != "" {
			termination["eventUri"] = patch.Termination.EventURI
		}
		if patch.Termination.EventCid != "" {
			termination["eventCid"] = patch.Termination.EventCid
		}
		if len(termination) > 0 {
			owned["termination"] = termination
		}
	}
	if len(owned) == 0 {
		return nil
	}
	body, err := statuspatch.Merge(map[string]any{"market": owned})
	if err != nil {
		return err
	}
	if err := v.Res.PatchStatus(ctx, v.Cluster, name, "", body); err != nil {
		return err
	}
	v.log().Info("a ComputeVM market field is recorded", "computevm", name)
	return nil
}

func (v *KCPVMs) PatchContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) error {
	if v == nil || v.Res == nil {
		return errors.New("bidder: the ComputeVM store is not configured")
	}
	value, err := json.Marshal(contract)
	if err != nil {
		return err
	}
	var raw any
	if err := json.Unmarshal(value, &raw); err != nil {
		return err
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/contract", "value": raw}})
	if err != nil {
		return err
	}
	return v.Res.PatchJSON(ctx, v.Cluster, name, patch)
}

func (v *KCPVMs) PatchAnnotations(ctx context.Context, name string, annotations map[string]string) error {
	if v == nil || v.Res == nil {
		return errors.New("bidder: the ComputeVM store is not configured")
	}
	if len(annotations) == 0 {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	if err != nil {
		return err
	}
	return v.Res.PatchMerge(ctx, v.Cluster, name, patch)
}

func (v *KCPVMs) WaitRunning(ctx context.Context, name string, timeout time.Duration) (*v1alpha1.ComputeVM, error) {
	timeout = opt.Positive(timeout, DefaultBootTimeout)
	deadline := time.Now().Add(timeout)
	for {
		vm, err := v.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		if vm != nil {
			switch vm.Status.Phase {
			case v1alpha1.ComputeVMPhaseRunning:
				return vm, nil
			case v1alpha1.ComputeVMPhaseFailed, v1alpha1.ComputeVMPhaseLost:
				return vm, fmt.Errorf("bidder: ComputeVM %s ended in phase %s: %s", name, vm.Status.Phase, vm.Status.Message)
			}
		}
		if time.Now().After(deadline) {
			return vm, fmt.Errorf("bidder: ComputeVM %s was not Running within %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return vm, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (v *KCPVMs) WaitAbsent(ctx context.Context, name string, timeout time.Duration) error {
	timeout = opt.Positive(timeout, DefaultReleaseTimeout)
	deadline := time.Now().Add(timeout)
	for {
		vm, err := v.Get(ctx, name)
		if err != nil {
			return err
		}
		if vm == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bidder: ComputeVM %s still exists after %s", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type KCPGrants struct {
	Res *kcpstore.ClusterResource[v1alpha1.GuestIdentityGrant]

	Cluster string
}

func NewKCPGrants(store *kcpstore.Store, cluster string) *KCPGrants {
	return &KCPGrants{Res: kcpstore.NewClusterResource[v1alpha1.GuestIdentityGrant](store, GuestIdentityGrantGVR), Cluster: cluster}
}

func (g *KCPGrants) Create(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error {
	if g == nil || g.Res == nil {
		return errors.New("bidder: the GuestIdentityGrant store is not configured")
	}
	grant.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "GuestIdentityGrant"}
	return g.Res.Create(ctx, g.Cluster, &grant)
}

func (g *KCPGrants) List(ctx context.Context) ([]v1alpha1.GuestIdentityGrant, error) {
	if g == nil || g.Res == nil {
		return nil, errors.New("bidder: the GuestIdentityGrant store is not configured")
	}
	return g.Res.List(ctx, g.Cluster)
}

func (g *KCPGrants) Delete(ctx context.Context, name string) error {
	if g == nil || g.Res == nil {
		return errors.New("bidder: the GuestIdentityGrant store is not configured")
	}
	return g.Res.Delete(ctx, g.Cluster, name)
}

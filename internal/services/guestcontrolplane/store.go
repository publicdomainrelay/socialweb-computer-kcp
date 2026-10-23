package guestcontrolplane

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

var (
	ComputeVMGVR = v1alpha1.ComputeVMGVR

	GuestIdentityGrantGVR = v1alpha1.GuestIdentityGrantGVR

	OpenBaoServerGVR = v1alpha1.OpenBaoServerGVR

	MarketIdentityGVR = v1alpha1.MarketIdentityGVR
)

type OpenBaoSource interface {
	Server(ctx context.Context, name string) (v1alpha1.OpenBaoServerStatus, error)
}

type KCPOpenBao struct {
	Res *kcpstore.ClusterResource[v1alpha1.OpenBaoServer]

	Cluster string
}

func (k *KCPOpenBao) Server(ctx context.Context, name string) (v1alpha1.OpenBaoServerStatus, error) {
	if k == nil || k.Res == nil {
		return v1alpha1.OpenBaoServerStatus{}, errors.New("guestcontrolplane: the OpenBao source is not configured")
	}
	server, err := k.Res.Get(ctx, k.Cluster, name)
	if err != nil {
		return v1alpha1.OpenBaoServerStatus{}, err
	}
	return server.Status, nil
}

type VMStore interface {
	Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error)

	List(ctx context.Context) ([]v1alpha1.ComputeVM, error)

	RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error
}

type GuestStore = guestidentity.GrantStore

type KCPVMs struct {
	Res *kcpstore.ClusterResource[v1alpha1.ComputeVM]

	Cluster string

	Handle string

	Log *slog.Logger
}

func NewKCPVMs(store *kcpstore.Store, cluster, handle string) *KCPVMs {
	return &KCPVMs{Res: kcpstore.NewClusterResource[v1alpha1.ComputeVM](store, ComputeVMGVR), Cluster: cluster, Handle: handle}
}

func (v *KCPVMs) Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("guestcontrolplane: the ComputeVM store is not configured")
	}
	vm, err := v.Res.Get(ctx, v.Cluster, name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return vm, err
}

func (v *KCPVMs) List(ctx context.Context) ([]v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("guestcontrolplane: the ComputeVM store is not configured")
	}
	return v.Res.List(ctx, v.Cluster)
}

func (v *KCPVMs) RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error {
	if v == nil || v.Res == nil {
		return errors.New("guestcontrolplane: the ComputeVM store is not configured")
	}
	if report.Address == "" && len(report.HostKeys) == 0 && report.ReportedAt == "" {
		return nil
	}
	current, err := v.Get(ctx, name)
	if err != nil {
		return err
	}
	if current == nil {
		return apierrors.NewNotFound(ComputeVMGVR.GroupResource(), name)
	}
	if held := computevm.GuestReportOf(current); held != nil && reflect.DeepEqual(*held, report) {
		return nil
	}
	guest, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&report)
	if err != nil {
		return err
	}
	return v.Res.ApplyStatus(ctx, v.Cluster, "ComputeVM", name, v1alpha1.FieldManagerComputeVMMarketGuest,
		map[string]any{"market": map[string]any{"guest": guest}})
}

type KCPGrants struct {
	Res *kcpstore.ClusterResource[v1alpha1.GuestIdentityGrant]

	Cluster string
}

func NewKCPGrants(store *kcpstore.Store, cluster string) *KCPGrants {
	return &KCPGrants{Res: kcpstore.NewClusterResource[v1alpha1.GuestIdentityGrant](store, GuestIdentityGrantGVR), Cluster: cluster}
}

func (g *KCPGrants) List(ctx context.Context) ([]v1alpha1.GuestIdentityGrant, error) {
	if g == nil || g.Res == nil {
		return nil, errors.New("guestcontrolplane: the GuestIdentityGrant store is not configured")
	}
	return g.Res.List(ctx, g.Cluster)
}

func (g *KCPGrants) Apply(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error {
	if g == nil || g.Res == nil {
		return errors.New("guestcontrolplane: the GuestIdentityGrant store is not configured")
	}
	grant.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "GuestIdentityGrant"}
	held, err := g.Res.Get(ctx, g.Cluster, grant.Name)
	if apierrors.IsNotFound(err) || err == nil && held == nil {
		return g.Res.Create(ctx, g.Cluster, &grant)
	}
	if err != nil {
		return err
	}
	spec, err := json.Marshal(grant.Spec)
	if err != nil {
		return err
	}
	var value any
	if err := json.Unmarshal(spec, &value); err != nil {
		return err
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": "/spec", "value": value}})
	if err != nil {
		return err
	}
	return g.Res.PatchJSON(ctx, g.Cluster, grant.Name, patch)
}

func (g *KCPGrants) Delete(ctx context.Context, name string) error {
	if g == nil || g.Res == nil {
		return errors.New("guestcontrolplane: the GuestIdentityGrant store is not configured")
	}
	return g.Res.Delete(ctx, g.Cluster, name)
}

func kcpStore(deps component.Deps, workspace string) (*kcpstore.Store, string, error) {
	if deps.KCP == nil {
		return nil, "", errors.New("guestcontrolplane: the component has no kcp clients")
	}
	workspace = cmp.Or(workspace, "root")
	client, err := deps.KCP.Client(workspace)
	if err != nil {
		return nil, "", fmt.Errorf("guestcontrolplane: kcp client for %s: %w", workspace, err)
	}
	cfg := client.RESTConfig()
	if cfg == nil || cfg.Host == "" {
		return nil, "", fmt.Errorf("guestcontrolplane: the kcp client for %s has no rest config", workspace)
	}
	store, err := kcpstore.New(kcpstore.Options{Host: cfg.Host, RestConfig: cfg})
	if err != nil {
		return nil, "", err
	}
	return store, workspace, nil
}

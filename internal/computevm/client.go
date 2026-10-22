package computevm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type MarketField string

const (
	MarketOnNetworkURI MarketField = "onNetworkUri"

	MarketAddress MarketField = "address"

	MarketHostKeys MarketField = "hostKeys"

	MarketHostKeysURI MarketField = "hostKeysUri"

	MarketReportedAt MarketField = "reportedAt"

	DefaultReleaseTimeout = 150 * time.Second
)

func ProviderMarketFields() []MarketField {
	return []MarketField{MarketOnNetworkURI, MarketHostKeysURI}
}

func GuestMarketFields() []MarketField {
	return []MarketField{MarketAddress, MarketReportedAt, MarketHostKeys}
}

func ownedMarketField(fields []MarketField, field MarketField) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func MergeMarket(held *v1alpha1.ComputeVMMarket, patch v1alpha1.ComputeVMMarket, fields []MarketField) v1alpha1.ComputeVMMarket {
	merged := v1alpha1.ComputeVMMarket{}
	if held != nil {
		if ownedMarketField(fields, MarketOnNetworkURI) {
			merged.OnNetworkUri = held.OnNetworkUri
		}
		if ownedMarketField(fields, MarketAddress) {
			merged.Address = held.Address
		}
		if ownedMarketField(fields, MarketHostKeys) {
			merged.HostKeys = append([]v1alpha1.ComputeVMHostKey(nil), held.HostKeys...)
		}
		if ownedMarketField(fields, MarketHostKeysURI) {
			merged.HostKeysUri = held.HostKeysUri
		}
		if ownedMarketField(fields, MarketReportedAt) {
			merged.ReportedAt = held.ReportedAt
		}
	}
	if ownedMarketField(fields, MarketOnNetworkURI) && patch.OnNetworkUri != "" {
		merged.OnNetworkUri = patch.OnNetworkUri
	}
	if ownedMarketField(fields, MarketAddress) && patch.Address != "" {
		merged.Address = patch.Address
	}
	if ownedMarketField(fields, MarketHostKeys) && len(patch.HostKeys) > 0 {
		merged.HostKeys = append([]v1alpha1.ComputeVMHostKey(nil), patch.HostKeys...)
	}
	if ownedMarketField(fields, MarketHostKeysURI) && patch.HostKeysUri != "" {
		merged.HostKeysUri = patch.HostKeysUri
	}
	if ownedMarketField(fields, MarketReportedAt) && patch.ReportedAt != "" {
		merged.ReportedAt = patch.ReportedAt
	}
	return merged
}

func MarketReportRKey(computevm, collection string) string {
	sum := sha256.Sum256([]byte(collection + "\x00" + computevm))
	return "vm-" + hex.EncodeToString(sum[:])[:32]
}

func marketEmpty(m v1alpha1.ComputeVMMarket) bool {
	return m.OnNetworkUri == "" && m.Address == "" && len(m.HostKeys) == 0 && m.HostKeysUri == "" && m.ReportedAt == ""
}

type VMs struct {
	Res dynamic.ResourceInterface

	Log *slog.Logger

	FieldManager string

	Fields []MarketField

	mu sync.Mutex
}

func NewVMs(res dynamic.ResourceInterface, fieldManager string, fields []MarketField) *VMs {
	return &VMs{Res: res, FieldManager: fieldManager, Fields: append([]MarketField(nil), fields...)}
}

func (v *VMs) log() *slog.Logger {
	if v.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return v.Log
}

func (v *VMs) Get(ctx context.Context, name string) (*v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("computevm: the ComputeVM client has no resource")
	}
	u, err := v.Res.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o v1alpha1.ComputeVM
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o); err != nil {
		return nil, fmt.Errorf("computevm: decoding ComputeVM %s: %w", name, err)
	}
	return &o, nil
}

func (v *VMs) List(ctx context.Context, opts metav1.ListOptions) ([]v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("computevm: the ComputeVM client has no resource")
	}
	list, err := v.Res.List(ctx, opts)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.ComputeVM, 0, len(list.Items))
	for i := range list.Items {
		var o v1alpha1.ComputeVM
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &o); err != nil {
			return nil, fmt.Errorf("computevm: decoding ComputeVM %s: %w", list.Items[i].GetName(), err)
		}
		out = append(out, o)
	}
	return out, nil
}

func (v *VMs) Apply(ctx context.Context, o *v1alpha1.ComputeVM) (*v1alpha1.ComputeVM, error) {
	if v == nil || v.Res == nil {
		return nil, errors.New("computevm: the ComputeVM client has no resource")
	}
	if o == nil || o.Name == "" {
		return nil, errors.New("computevm: applying a ComputeVM with no name")
	}
	u, err := encode(o, "ComputeVM")
	if err != nil {
		return nil, err
	}
	delete(u.Object, "status")
	applied, err := v.Res.Apply(ctx, o.Name, u, metav1.ApplyOptions{FieldManager: v.FieldManager, Force: true})
	if err != nil {
		return nil, err
	}
	var out v1alpha1.ComputeVM
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(applied.Object, &out); err != nil {
		return nil, fmt.Errorf("computevm: decoding the applied ComputeVM %s: %w", o.Name, err)
	}
	return &out, nil
}

func (v *VMs) ApplyContract(ctx context.Context, name string, contract v1alpha1.ComputeVMContract) (*v1alpha1.ComputeVM, error) {
	held, err := v.Get(ctx, name)
	if err != nil || held == nil {
		return nil, err
	}
	body := map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "ComputeVM",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"contract": contract},
	}
	applied, err := v.Res.Apply(ctx, name, &unstructured.Unstructured{Object: body},
		metav1.ApplyOptions{FieldManager: v1alpha1.FieldManagerComputeVMContract, Force: true})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out v1alpha1.ComputeVM
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(applied.Object, &out); err != nil {
		return nil, fmt.Errorf("computevm: decoding the applied contract of %s: %w", name, err)
	}
	return &out, nil
}

func (v *VMs) Delete(ctx context.Context, name string) (bool, error) {
	if v == nil || v.Res == nil {
		return false, errors.New("computevm: the ComputeVM client has no resource")
	}
	err := v.Res.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (v *VMs) RecordMarket(ctx context.Context, name string, patch v1alpha1.ComputeVMMarket) error {
	if v == nil || v.Res == nil {
		return errors.New("computevm: the ComputeVM client has no resource")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	current, err := v.Get(ctx, name)
	if err != nil {
		return err
	}
	if current == nil {
		return fmt.Errorf("%w: %s", apierrors.NewNotFound(GVR.GroupResource(), name), name)
	}
	merged := MergeMarket(current.Status.Market, patch, v.Fields)
	if reflect.DeepEqual(current.Status.Market, &merged) {
		return nil
	}
	if current.Status.Market == nil && marketEmpty(merged) {
		return nil
	}
	market, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&merged)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"market": market}}}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind("ComputeVM")
	u.SetName(name)
	if _, err := v.Res.ApplyStatus(ctx, name, u, metav1.ApplyOptions{FieldManager: v.FieldManager, Force: true}); err != nil {
		return err
	}
	v.log().Info("a ComputeVM market field is recorded", "computevm", name, "fieldManager", v.FieldManager)
	return nil
}

func (v *VMs) WaitAbsent(ctx context.Context, name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultReleaseTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		o, err := v.Get(ctx, name)
		if err != nil {
			return err
		}
		if o == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("computevm: %s still exists after %s; the controller holds its finalizer until the VM is stopped and its work dir removed", name, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (v *VMs) Release(ctx context.Context, name string, timeout time.Duration) (bool, error) {
	deleted, err := v.Delete(ctx, name)
	if err != nil || !deleted {
		return deleted, err
	}
	if err := v.WaitAbsent(ctx, name, timeout); err != nil {
		return true, err
	}
	v.log().Info("a ComputeVM is released", "computevm", name)
	return true, nil
}

func (v *VMs) ReleaseOrphansOfAccept(ctx context.Context, acceptKey, handle string, keep map[string]bool) ([]string, error) {
	if acceptKey == "" || handle == "" {
		return nil, nil
	}
	list, err := v.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var released []string
	for i := range list {
		o := &list[i]
		switch {
		case o.DeletionTimestamp != nil, keep[o.Name], o.Spec.Owner.Handle != handle:
			continue
		case AcceptKeyOf(o) != acceptKey, ContractReceipt(o) != "":
			continue
		}
		deleted, err := v.Delete(ctx, o.Name)
		if err != nil {
			return released, err
		}
		if deleted {
			released = append(released, o.Name)
			v.log().Warn("a ComputeVM of an accept with no receipt is released", "computevm", o.Name, "accept", acceptKey)
		}
	}
	return released, nil
}

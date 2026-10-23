package kube

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ListPage[T any](ctx context.Context, r Resource, opts metav1.ListOptions) ([]T, metav1.ListMeta, error) {
	ri, err := r.iface()
	if err != nil {
		return nil, metav1.ListMeta{}, err
	}
	l, err := ri.List(ctx, opts)
	if err != nil {
		return nil, metav1.ListMeta{}, err
	}
	out := make([]T, 0, len(l.Items))
	for i := range l.Items {
		v, err := Decode[T](l.Items[i].Object)
		if err != nil {
			return nil, metav1.ListMeta{}, err
		}
		out = append(out, *v)
	}
	meta := metav1.ListMeta{ResourceVersion: l.GetResourceVersion(), Continue: l.GetContinue(), RemainingItemCount: l.GetRemainingItemCount()}
	return out, meta, nil
}

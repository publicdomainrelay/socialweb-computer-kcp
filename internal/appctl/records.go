package appctl

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var AtprotoRecordGVR = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "atprotorecords"}

func (ctl *Controller) releaseRecords(ctx context.Context, recordsWs, podsWs, owner string) (int, error) {
	ri, err := ctl.client(recordsWs, AtprotoRecordGVR)
	if err != nil {
		return 0, err
	}
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || (err == nil && len(list.Items) == 0) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pdsUp := false
	if v, ok, err := ctl.GetComponentIn(ctx, podsWs, ComponentKindPds, PdsComponent); err == nil && ok && v.Status.Ready {
		pdsUp = true
	}
	for _, o := range list.Items {
		switch {
		case o.GetDeletionTimestamp() == nil:
			err = ri.Delete(ctx, o.GetName(), metav1.DeleteOptions{})
		case !pdsUp:
			_, err = ri.Patch(ctx, o.GetName(), types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return len(list.Items), err
		}
	}
	return len(list.Items), nil
}

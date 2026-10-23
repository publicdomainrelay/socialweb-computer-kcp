package exposure

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	GroupName = "exposure.socialweb.computer"
	Version   = "v1alpha1"

	FieldManager = "exposure-controller"

	ServiceFinalizer  = "exposure.socialweb.computer/service"
	ExposureFinalizer = "exposure.socialweb.computer/exposure"

	LabelService = "exposure.socialweb.computer/service"

	ConditionHandlerRegistered = "HandlerRegistered"
	ConditionReady             = "Ready"
	ConditionIngressProvider   = "IngressProvider"

	DefaultIngressName = "dispatcher"

	DefaultRetryAfter = 2 * time.Second
)

var (
	ServiceGVR       = exposurev1alpha1.ServiceGVR
	ExposureGVR      = exposurev1alpha1.ExposureGVR
	ComponentLinkGVR = exposurev1alpha1.ComponentLinkGVR
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type watcher struct {
	workspace string
	dynamic   DynamicFactory
	gvr       schema.GroupVersionResource
	log       *slog.Logger
	retry     time.Duration
	reconcile func(context.Context, dynamic.Interface, *unstructured.Unstructured) error
	deleted   func(string)
}

func (w *watcher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := w.round(ctx); err != nil && ctx.Err() == nil {
			w.log.Warn("watching exposure resources", "gvr", w.gvr.Resource, "workspace", w.workspace, "err", err.Error())
		}
		if !clock.Sleep(ctx, opt.Positive(w.retry, DefaultRetryAfter)) {
			return nil
		}
	}
	return nil
}

func (w *watcher) round(ctx context.Context) error {
	dyn, err := w.dynamic(w.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(w.gvr)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := w.reconcile(bus.WithObject(ctx, w.workspace, &list.Items[i]), dyn, &list.Items[i]); err != nil && ctx.Err() == nil {
			w.log.Warn("reconciling an exposure resource", "gvr", w.gvr.Resource, "workspace", w.workspace,
				"name", list.Items[i].GetName(), "err", err.Error())
		}
	}
	options := metav1.ListOptions{ResourceVersion: list.GetResourceVersion()}
	wc, err := ri.Watch(ctx, options)
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wc.Stop()
	timer := time.NewTimer(w.retry)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case event, ok := <-wc.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Bookmark {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			if event.Type == watch.Deleted {
				if w.deleted != nil {
					w.deleted(u.GetName())
				}
				continue
			}
			if err := w.reconcile(bus.WithObject(ctx, w.workspace, u), dyn, u); err != nil && ctx.Err() == nil {
				w.log.Warn("reconciling an exposure resource", "gvr", w.gvr.Resource, "workspace", w.workspace,
					"name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func ensureMetadata(ctx context.Context, ri dynamic.ResourceInterface, name, finalizer string, wantFinalizer bool, labels map[string]string) error {
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		changed := false
		if finalizer != "" {
			finalizers := cur.GetFinalizers()
			has := containsString(finalizers, finalizer)
			switch {
			case wantFinalizer && !has:
				finalizers = append(finalizers, finalizer)
				cur.SetFinalizers(finalizers)
				changed = true
			case !wantFinalizer && has:
				kept := finalizers[:0]
				for _, value := range finalizers {
					if value != finalizer {
						kept = append(kept, value)
					}
				}
				cur.SetFinalizers(kept)
				changed = true
			}
		}
		for key, value := range labels {
			if cur.GetLabels()[key] != value {
				existing := cur.GetLabels()
				if existing == nil {
					existing = map[string]string{}
				}
				existing[key] = value
				cur.SetLabels(existing)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("exposure: the metadata of %s stayed in conflict", name)
}

func writeStatus(ctx context.Context, ri dynamic.ResourceInterface, name string, status any) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return err
	}
	if object == nil {
		object = map[string]any{}
	}
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.GetDeletionTimestamp() != nil {
			return nil
		}
		existing, _, _ := unstructured.NestedMap(cur.Object, "status")
		if reflect.DeepEqual(existing, object) {
			return nil
		}
		cur.Object["status"] = object
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("exposure: the status of %s stayed in conflict", name)
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func normalizeName(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "/"); index >= 0 {
		value = value[index+1:]
	}
	return strings.TrimSpace(value)
}

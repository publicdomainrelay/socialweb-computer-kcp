package hostlifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const DefaultFieldManager = "hostlifecycle"

const (
	ConditionInstalled   = "Installed"
	ConditionProgressing = "Progressing"
	ConditionCurrent     = "Current"
	ConditionSelfCheck   = "SelfCheck"
	ConditionGC          = "GC"
)

var (
	KcpInstallationGVR = v1alpha1.KcpInstallationGVR
	HostInstallGVR     = v1alpha1.HostInstallGVR
	HostReleaseGVR     = v1alpha1.HostReleaseGVR
)

type ReconcileFunc func(context.Context, dynamic.Interface, *unstructured.Unstructured) error

type Hooks interface {
	Add(gvr schema.GroupVersionResource, reconcile ReconcileFunc)
}

func Register(h Hooks, c *Controller) {
	if h == nil || c == nil {
		return
	}
	h.Add(KcpInstallationGVR, c.ReconcileKcpInstallation)
	h.Add(HostInstallGVR, c.ReconcileHostInstall)
	h.Add(HostReleaseGVR, c.ReconcileHostRelease)
}

type Options struct {
	Authority    Authority
	Log          *slog.Logger
	FieldManager string
}

type Controller struct {
	authority    Authority
	log          *slog.Logger
	fieldManager string
}

func New(o Options) (*Controller, error) {
	if o.Authority == nil {
		return nil, errors.New("hostlifecycle: an authority is required")
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	fieldManager := o.FieldManager
	if fieldManager == "" {
		fieldManager = DefaultFieldManager
	}
	return &Controller{authority: o.Authority, log: log, fieldManager: fieldManager}, nil
}

func (c *Controller) writeStatus(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, name string, status any) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		return err
	}
	if object == nil {
		object = map[string]any{}
	}
	ri := dyn.Resource(gvr)
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
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: c.fieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("hostlifecycle: the status of %s stayed in conflict", name)
}

func specInto(u *unstructured.Unstructured, out any) error {
	spec, found, err := unstructured.NestedMap(u.Object, "spec")
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(spec, out)
}

func authorityFailure(conditions *[]metav1.Condition, generation int64, message string, kinds ...string) {
	for _, kind := range kinds {
		conds.SetStatus(conditions, generation, kind, metav1.ConditionUnknown, "AuthorityUnreadable", message)
	}
	conds.SetStatus(conditions, generation, "Ready", metav1.ConditionFalse, "AuthorityUnreadable", message)
}

const authorityUnreadable = "the host authority could not be read; the controller retries"

package childapplication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
)

const (
	ConditionReady       = "Ready"
	ConditionProgressing = "Progressing"
	ConditionFailed      = "Failed"
	ConditionOrphaned    = "Orphaned"

	Finalizer = "childapplication.socialweb.computer/workspace"

	FieldManager = "childapplication-controller"

	DefaultWorkspace = "root"

	DefaultRetryAfter = 2 * time.Second

	devWorkspacePrefix = "root:dev"

	componentWorkspace = "app"

	appWorkspaceLabel = "apps.socialweb.computer/application"

	hostProcessUnitLabel = "socialweb.computer/unit"
)

var (
	ChildApplicationGVR = v1alpha1.ChildApplicationGVR
	MarketNetworkGVR    = v1alpha1.MarketNetworkGVR
	HostProcessGVR      = v1alpha1.HostProcessGVR

	WorkspaceGVR = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type ReconcileFunc func(context.Context, dynamic.Interface, *unstructured.Unstructured) error

type Registrar interface {
	Register(gvr schema.GroupVersionResource, reconcile ReconcileFunc) error
}

type Options struct {
	Dynamic DynamicFactory

	Workspace string

	NetworkWorkspace string

	Log *slog.Logger

	FieldManager string

	RetryAfter time.Duration
}

type Controller struct {
	dynamic          DynamicFactory
	workspace        string
	networkWorkspace string
	fieldManager     string
	log              *slog.Logger
	retry            time.Duration
}

func New(o Options) (*Controller, error) {
	if o.Dynamic == nil {
		return nil, errors.New("childapplication: a dynamic client factory is required")
	}
	workspace := o.Workspace
	if workspace == "" {
		workspace = DefaultWorkspace
	}
	networkWorkspace := o.NetworkWorkspace
	if networkWorkspace == "" {
		networkWorkspace = workspace
	}
	if !logicalCluster(workspace) {
		return nil, fmt.Errorf("childapplication: workspace %q is not a logical cluster path", workspace)
	}
	if !logicalCluster(networkWorkspace) {
		return nil, fmt.Errorf("childapplication: network workspace %q is not a logical cluster path", networkWorkspace)
	}
	fieldManager := o.FieldManager
	if fieldManager == "" {
		fieldManager = FieldManager
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	retry := o.RetryAfter
	if retry <= 0 {
		retry = DefaultRetryAfter
	}
	return &Controller{dynamic: o.Dynamic, workspace: workspace, networkWorkspace: networkWorkspace,
		fieldManager: fieldManager, log: log, retry: retry}, nil
}

func Register(reg Registrar, o Options) (*Controller, error) {
	c, err := New(o)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return c, nil
	}
	if err := reg.Register(ChildApplicationGVR, c.Reconcile); err != nil {
		return nil, fmt.Errorf("childapplication: registering the child application reconciler: %w", err)
	}
	return c, nil
}

func (c *Controller) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		if err := c.round(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching child applications", "workspace", c.workspace, "err", err.Error())
		}
		if !clock.Sleep(ctx, c.retry) {
			return nil
		}
	}
	return nil
}

func (c *Controller) round(ctx context.Context) error {
	if err := c.Ensure(ctx); err != nil && ctx.Err() == nil {
		c.log.Warn("ensuring one child application per network app", "err", err.Error())
	}
	dyn, err := c.client(c.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(ChildApplicationGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := c.Reconcile(ctx, dyn, &list.Items[i]); err != nil && ctx.Err() == nil {
			c.log.Warn("reconciling a child application", "name", list.Items[i].GetName(), "err", err.Error())
		}
	}
	wc, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer wc.Stop()
	timer := time.NewTimer(c.retry)
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
				continue
			}
			if err := c.Reconcile(ctx, dyn, u); err != nil && ctx.Err() == nil {
				c.log.Warn("reconciling a child application", "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func (c *Controller) client(workspace string) (dynamic.Interface, error) {
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return nil, fmt.Errorf("childapplication: the client for %s: %w", workspace, err)
	}
	if dyn == nil {
		return nil, fmt.Errorf("childapplication: the client for %s is nil", workspace)
	}
	return dyn, nil
}

package remotepds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
)

const (
	FieldManager = "remotepds-controller"

	ConditionReady = "Ready"

	ConditionFailed = "Failed"

	DefaultRetryAfter = 2 * time.Second

	DefaultTimeout = 10 * time.Second

	DefaultCheckInterval = 30 * time.Second
)

var RemotePDSGVR = pdsv1alpha1.GroupVersion.WithResource("remotepds")

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type ReconcileFunc func(context.Context, dynamic.Interface, *unstructured.Unstructured) error

type Registrar interface {
	Register(gvr schema.GroupVersionResource, reconcile ReconcileFunc) error
}

type CABundleFunc func(ref string) ([]byte, error)

type Options struct {
	Workspace string

	Dynamic DynamicFactory

	Log *slog.Logger

	CABundle CABundleFunc

	Timeout time.Duration

	RetryAfter time.Duration

	CheckInterval time.Duration
}

type Controller struct {
	workspace string

	dynamic DynamicFactory

	log *slog.Logger

	caBundle CABundleFunc

	timeout time.Duration

	retry time.Duration

	checkInterval time.Duration
}

func New(opts Options) (*Controller, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("remotepds: a workspace is required")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("remotepds: a dynamic client factory is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	retry := opts.RetryAfter
	if retry <= 0 {
		retry = DefaultRetryAfter
	}
	checkInterval := opts.CheckInterval
	if checkInterval <= 0 {
		checkInterval = DefaultCheckInterval
	}
	return &Controller{
		workspace:     workspace,
		dynamic:       opts.Dynamic,
		log:           log.With("component", "remotepds-controller"),
		caBundle:      opts.CABundle,
		timeout:       timeout,
		retry:         retry,
		checkInterval: checkInterval,
	}, nil
}

func Register(reg Registrar, opts Options) (*Controller, error) {
	ctl, err := New(opts)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return ctl, nil
	}
	if err := reg.Register(RemotePDSGVR, ctl.Reconcile); err != nil {
		return nil, fmt.Errorf("remotepds: registering the RemotePDS reconciler: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for ctx.Err() == nil {
		if err := c.watch(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching RemotePDS objects", "workspace", c.workspace, "err", err.Error())
		}
		if !sleep(ctx, c.retry) {
			return nil
		}
	}
	return nil
}

func (c *Controller) watch(ctx context.Context) error {
	dyn, err := c.dynamic(c.workspace)
	if err != nil {
		return fmt.Errorf("remotepds: the client for %s: %w", c.workspace, err)
	}
	if dyn == nil {
		return fmt.Errorf("remotepds: the client for %s is nil", c.workspace)
	}
	ri := dyn.Resource(RemotePDSGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := c.Reconcile(ctx, dyn, &list.Items[i]); err != nil && ctx.Err() == nil {
			c.log.Warn("reconciling a RemotePDS", "workspace", c.workspace, "name", list.Items[i].GetName(), "err", err.Error())
		}
	}
	w, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer w.Stop()
	timer := time.NewTimer(c.retry)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case event, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Deleted || event.Type == watch.Bookmark {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			if err := c.Reconcile(ctx, dyn, u); err != nil && ctx.Err() == nil {
				c.log.Warn("reconciling a RemotePDS", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

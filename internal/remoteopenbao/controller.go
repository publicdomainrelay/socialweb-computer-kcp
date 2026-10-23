package remoteopenbao

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultaccess"
)

const (
	FieldManager = "remoteopenbao-controller"

	ConditionReachable = "Reachable"

	ConditionAuthenticated = "Authenticated"

	ConditionUnsealed = "Unsealed"

	ConditionAuthorized = "Authorized"

	DefaultRetryAfter = 2 * time.Second

	DefaultMaxRetryAfter = 5 * time.Minute

	DefaultTimeout = 10 * time.Second

	DefaultCheckInterval = 30 * time.Second
)

var RemoteOpenBaoGVR = v1alpha1.RemoteOpenBaoGVR

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type ReconcileFunc func(context.Context, dynamic.Interface, *unstructured.Unstructured) error

type Registrar interface {
	Register(gvr schema.GroupVersionResource, reconcile ReconcileFunc) error
}

type Options struct {
	Workspace string

	Dynamic DynamicFactory

	Resolver *vaultaccess.Resolver

	Log *slog.Logger

	Timeout time.Duration

	RetryAfter time.Duration

	MaxRetryAfter time.Duration

	CheckInterval time.Duration

	Now func() time.Time
}

type attemptState struct {
	generation int64

	failures int

	until time.Time
}

type Controller struct {
	workspace string

	dynamic DynamicFactory

	resolver *vaultaccess.Resolver

	log *slog.Logger

	timeout time.Duration

	retry time.Duration

	maxRetry time.Duration

	checkInterval time.Duration

	now func() time.Time

	mu sync.Mutex

	attempts map[string]attemptState
}

func New(opts Options) (*Controller, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("remoteopenbao: a workspace is required")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("remoteopenbao: a dynamic client factory is required")
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
	maxRetry := opts.MaxRetryAfter
	if maxRetry <= 0 {
		maxRetry = DefaultMaxRetryAfter
	}
	if maxRetry < retry {
		maxRetry = retry
	}
	checkInterval := opts.CheckInterval
	if checkInterval <= 0 {
		checkInterval = DefaultCheckInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Controller{
		workspace:     workspace,
		dynamic:       opts.Dynamic,
		resolver:      opts.Resolver,
		log:           log.With("component", "remoteopenbao-controller"),
		timeout:       timeout,
		retry:         retry,
		maxRetry:      maxRetry,
		checkInterval: checkInterval,
		now:           now,
		attempts:      map[string]attemptState{},
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
	if err := reg.Register(RemoteOpenBaoGVR, ctl.Reconcile); err != nil {
		return nil, fmt.Errorf("remoteopenbao: registering the RemoteOpenBao reconciler: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for ctx.Err() == nil {
		if err := c.watch(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching RemoteOpenBao objects", "workspace", c.workspace, "err", err.Error())
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
		return fmt.Errorf("remoteopenbao: the client for %s: %w", c.workspace, err)
	}
	if dyn == nil {
		return fmt.Errorf("remoteopenbao: the client for %s is nil", c.workspace)
	}
	ri := dyn.Resource(RemoteOpenBaoGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := c.Reconcile(ctx, dyn, &list.Items[i]); err != nil && ctx.Err() == nil {
			c.log.Warn("reconciling a RemoteOpenBao", "workspace", c.workspace, "name", list.Items[i].GetName(), "err", err.Error())
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
				c.log.Warn("reconciling a RemoteOpenBao", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func (c *Controller) waiting(name string, generation int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, ok := c.attempts[name]
	if !ok || state.generation != generation {
		return false
	}
	return c.now().Before(state.until)
}

func (c *Controller) fail(name string, generation int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.attempts[name]
	if state.generation != generation {
		state = attemptState{generation: generation}
	}
	state.failures++
	delay := c.retry
	for i := 1; i < state.failures && delay < c.maxRetry; i++ {
		delay *= 2
		if delay > c.maxRetry {
			delay = c.maxRetry
		}
	}
	state.until = c.now().Add(delay)
	c.attempts[name] = state
}

func (c *Controller) succeed(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.attempts, name)
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

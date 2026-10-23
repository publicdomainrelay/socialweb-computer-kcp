package component

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"k8s.io/apimachinery/pkg/types"
)

const (
	DefaultResync    = 30 * time.Second
	DefaultQueueSize = 256
)

type EventType string

const (
	EventAdded    EventType = "Added"
	EventModified EventType = "Modified"
	EventDeleted  EventType = "Deleted"
)

type Event struct {
	Type EventType
	Spec ComponentSpec
}

type Source interface {
	List(ctx context.Context) ([]ComponentSpec, error)
	Watch(ctx context.Context) (<-chan Event, error)
}

type StatusWriter interface {
	WriteStatus(ctx context.Context, spec ComponentSpec, status ComponentStatus) error
}

type ControllerConfig struct {
	Runtime   *Runtime
	Source    Source
	Status    StatusWriter
	Log       *slog.Logger
	Workers   int
	Resync    time.Duration
	QueueSize int
}

type Controller struct {
	runtime *Runtime
	source  Source
	status  StatusWriter
	log     *slog.Logger
	workers int
	resync  time.Duration

	mu      sync.Mutex
	pending map[types.UID]ComponentSpec
	work    chan types.UID
}

func NewController(cfg ControllerConfig) (*Controller, error) {
	switch {
	case cfg.Runtime == nil:
		return nil, errors.New("component: the controller needs a runtime")
	case cfg.Source == nil:
		return nil, errors.New("component: the controller needs a component source")
	case cfg.Status == nil:
		return nil, errors.New("component: the controller needs a status writer")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	cfg.Workers = opt.Positive(cfg.Workers, 1)
	cfg.Resync = opt.Positive(cfg.Resync, DefaultResync)
	cfg.QueueSize = opt.Positive(cfg.QueueSize, DefaultQueueSize)
	c := &Controller{
		runtime: cfg.Runtime,
		source:  cfg.Source,
		status:  cfg.Status,
		log:     cfg.Log,
		workers: cfg.Workers,
		resync:  cfg.Resync,
		pending: map[types.UID]ComponentSpec{},
		work:    make(chan types.UID, cfg.QueueSize),
	}
	cfg.Runtime.SetStatusCallback(c.writeStatus)
	return c, nil
}

func (c *Controller) Run(ctx context.Context) error {
	if err := c.runtime.Start(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	events, err := c.source.Watch(runCtx)
	if err != nil {
		stopErr := c.shutdown(ctx)
		return errors.Join(fmt.Errorf("component: watching components: %w", err), stopErr)
	}

	var wg sync.WaitGroup
	for range c.workers {
		wg.Go(func() { c.worker(runCtx) })
	}

	specs, err := c.source.List(runCtx)
	if err != nil {
		cancel()
		wg.Wait()
		drain(events)
		stopErr := c.shutdown(ctx)
		return errors.Join(fmt.Errorf("component: listing components: %w", err), stopErr)
	}
	for _, spec := range specs {
		c.enqueue(runCtx, spec)
	}

	ticker := time.NewTicker(c.resync)
	runErr := c.loop(runCtx, events, ticker)
	ticker.Stop()
	cancel()
	wg.Wait()
	drain(events)
	stopErr := c.shutdown(ctx)
	if runErr != nil {
		return runErr
	}
	return stopErr
}

func drain(events <-chan Event) {
	for range events {
	}
}

func (c *Controller) Reconcile(ctx context.Context, spec ComponentSpec) error {
	_, err := c.runtime.Reconcile(ctx, spec)
	return err
}

func (c *Controller) loop(ctx context.Context, events <-chan Event, ticker *time.Ticker) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				return errors.New("component: the component watch closed")
			}
			c.handleEvent(ctx, event)
		case <-ticker.C:
			specs, err := c.source.List(ctx)
			if err != nil {
				c.log.Warn("component: listing components failed", "err", err)
				continue
			}
			for _, spec := range specs {
				c.enqueue(ctx, spec)
			}
		}
	}
}

func (c *Controller) handleEvent(ctx context.Context, event Event) {
	switch event.Type {
	case EventAdded, EventModified:
		c.enqueue(ctx, event.Spec)
	case EventDeleted:
		spec := event.Spec
		spec.Deleting = true
		c.enqueue(ctx, spec)
	default:
		c.log.Warn("component: unknown component event", "type", event.Type, "uid", event.Spec.UID)
	}
}

func (c *Controller) enqueue(ctx context.Context, spec ComponentSpec) {
	if spec.UID == "" {
		c.log.Warn("component: dropping a component spec without a uid", "kind", spec.Kind, "name", spec.Name, "workspace", spec.Workspace)
		return
	}
	c.mu.Lock()
	if _, ok := c.pending[spec.UID]; ok {
		c.pending[spec.UID] = spec
		c.mu.Unlock()
		return
	}
	c.pending[spec.UID] = spec
	c.mu.Unlock()
	select {
	case c.work <- spec.UID:
	case <-ctx.Done():
	}
}

func (c *Controller) take(uid types.UID) (ComponentSpec, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	spec, ok := c.pending[uid]
	if ok {
		delete(c.pending, uid)
	}
	return spec, ok
}

func (c *Controller) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case uid := <-c.work:
			spec, ok := c.take(uid)
			if !ok {
				continue
			}
			if err := c.Reconcile(ctx, spec); err != nil {
				c.log.Warn("component: reconcile failed",
					"uid", spec.UID,
					"component", spec.Kind+"/"+spec.Name,
					"workspace", spec.Workspace,
					"err", err)
			}
		}
	}
}

func (c *Controller) writeStatus(ctx context.Context, spec ComponentSpec, status ComponentStatus) error {
	if c.status == nil {
		return nil
	}
	if err := c.status.WriteStatus(ctx, spec, status); err != nil {
		c.log.Warn("component: writing status failed",
			"uid", spec.UID,
			"component", spec.Kind+"/"+spec.Name,
			"workspace", spec.Workspace,
			"phase", status.Phase,
			"err", err)
		return err
	}
	return nil
}

func (c *Controller) shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultStopTimeout)
	defer cancel()
	return c.runtime.Stop(ctx)
}

package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

const (
	GroupName = "events.socialweb.computer"
	Version   = "v1alpha1"

	FieldManager = "eventstream-controller"

	DefaultRetryAfter       = 2 * time.Second
	DefaultStatusInterval   = 5 * time.Second
	DefaultReconnectDelay   = time.Second
	DefaultMaxReconnectWait = 30 * time.Second

	pdsCacheTTL = 2 * time.Minute
)

var EventStreamGVR = schema.GroupVersionResource{Group: GroupName, Version: Version, Resource: "eventstreams"}

var RemotePDSGVR = schema.GroupVersionResource{Group: "pds.socialweb.computer", Version: "v1alpha1", Resource: "remotepds"}

var RecordWatchGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "atprotorecordwatches"}

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Workspace string
	Dynamic   DynamicFactory
	Resolver  *pdsclient.Resolver
	Handlers  *HandlerRegistry
	Log       *slog.Logger

	Directory       identity.Directory
	PLCDirectoryURL string
	HTTPClient      *http.Client

	RetryAfter       time.Duration
	StatusInterval   time.Duration
	ReconnectDelay   time.Duration
	MaxReconnectWait time.Duration
}

type Controller struct {
	opts     Options
	log      *slog.Logger
	handlers *HandlerRegistry

	directory identity.Directory
	http      *http.Client

	mu       sync.Mutex
	streams  map[string]*streamRuntime
	pdsCache map[syntax.DID]pdsCacheEntry
	verifyAt map[string]time.Time
}

type pdsCacheEntry struct {
	pds       pdsclient.PDS
	fetchedAt time.Time
}

type streamRuntime struct {
	uid         types.UID
	generation  int64
	fingerprint string
	cancel      context.CancelFunc
	done        chan struct{}
}

func Register(opts Options) (*Controller, error) {
	return New(opts)
}

func New(opts Options) (*Controller, error) {
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	if opts.Workspace == "" {
		return nil, errors.New("eventstream: a workspace is required")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("eventstream: a dynamic client factory is required")
	}
	if opts.Resolver == nil {
		return nil, errors.New("eventstream: a pdsclient resolver is required")
	}
	if opts.Handlers == nil {
		opts.Handlers = NewHandlerRegistry()
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	if opts.StatusInterval <= 0 {
		opts.StatusInterval = DefaultStatusInterval
	}
	if opts.ReconnectDelay <= 0 {
		opts.ReconnectDelay = DefaultReconnectDelay
	}
	if opts.MaxReconnectWait < opts.ReconnectDelay {
		opts.MaxReconnectWait = DefaultMaxReconnectWait
	}
	httpClient := opts.HTTPClient
	if httpClient == nil && opts.Resolver != nil && opts.Resolver.Handlers != nil {
		httpClient = opts.Resolver.Handlers.HTTPClient()
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	httpClient = httpClientWithFallback(httpClient)
	directory := opts.Directory
	if directory == nil {
		plcURL := strings.TrimSpace(opts.PLCDirectoryURL)
		if plcURL == "" {
			plcURL = "http://" + kcp.ServiceFQDN("plc", "default", opts.Workspace, kcp.DefaultServiceDomain)
		}
		local := &identity.BaseDirectory{
			PLCURL:                 plcURL,
			HTTPClient:             *httpClient,
			PLCClient:              httpClient,
			SkipHandleVerification: true,
		}
		public := &identity.BaseDirectory{
			PLCURL:                 identity.DefaultPLCURL,
			HTTPClient:             *httpClient,
			PLCClient:              httpClient,
			SkipHandleVerification: true,
		}
		directory = fallbackDirectory{local, public}
	}
	return &Controller{
		opts:      opts,
		log:       opts.Log.With("component", "eventstream-controller"),
		handlers:  opts.Handlers,
		streams:   map[string]*streamRuntime{},
		directory: directory,
		http:      httpClient,
		pdsCache:  map[syntax.DID]pdsCacheEntry{},
		verifyAt:  map[string]time.Time{},
	}, nil
}

func (c *Controller) Handlers() *HandlerRegistry {
	return c.handlers
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		c.runEventStreams(runCtx)
	}()
	go func() {
		defer workers.Done()
		c.runRecordWatches(runCtx)
	}()
	<-runCtx.Done()
	c.stopAll()
	workers.Wait()
	return nil
}

func (c *Controller) runEventStreams(ctx context.Context) {
	for ctx.Err() == nil {
		if err := c.round(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching EventStreams", "workspace", c.opts.Workspace, "err", err.Error())
		}
		if !sleep(ctx, c.opts.RetryAfter) {
			return
		}
	}
}

func (c *Controller) round(ctx context.Context) error {
	dyn, err := c.opts.Dynamic(c.opts.Workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(EventStreamGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if err := c.reconcile(ctx, dyn, item); err != nil && ctx.Err() == nil {
			c.log.Warn("reconciling an EventStream", "name", item.GetName(), "err", err.Error())
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
	timer := time.NewTimer(c.opts.RetryAfter)
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
				c.forget(u.GetName())
				continue
			}
			if err := c.reconcile(ctx, dyn, u); err != nil && ctx.Err() == nil {
				c.log.Warn("reconciling an EventStream", "name", u.GetName(), "err", err.Error())
			}
		}
	}
}

func (c *Controller) reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	es := &eventsv1alpha1.EventStream{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, es); err != nil {
		return fmt.Errorf("eventstream: decoding EventStream %s: %w", u.GetName(), err)
	}
	fingerprint, err := specFingerprint(es.Spec)
	if err != nil {
		return err
	}
	c.mu.Lock()
	current := c.streams[es.Name]
	if current != nil && current.uid == es.UID && current.generation == es.Generation && current.fingerprint == fingerprint {
		c.mu.Unlock()
		return nil
	}
	if current != nil {
		current.cancel()
	}
	streamCtx, cancel := context.WithCancel(ctx)
	state := &streamRuntime{uid: es.UID, generation: es.Generation, fingerprint: fingerprint, cancel: cancel, done: make(chan struct{})}
	c.streams[es.Name] = state
	stream := newEventStream(c, es, dyn)
	c.mu.Unlock()
	go func() {
		defer close(state.done)
		stream.run(streamCtx)
	}()
	return nil
}

func (c *Controller) forget(name string) {
	c.mu.Lock()
	runtime := c.streams[name]
	delete(c.streams, name)
	c.mu.Unlock()
	if runtime != nil {
		runtime.cancel()
	}
}

func (c *Controller) stopAll() {
	c.mu.Lock()
	streams := c.streams
	c.streams = map[string]*streamRuntime{}
	c.mu.Unlock()
	for _, runtime := range streams {
		runtime.cancel()
	}
	deadline := time.After(DefaultStatusInterval)
	for _, runtime := range streams {
		select {
		case <-runtime.done:
		case <-deadline:
			return
		}
	}
}

func specFingerprint(spec eventsv1alpha1.EventStreamSpec) (string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("eventstream: fingerprinting the spec: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sleep(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = DefaultRetryAfter
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

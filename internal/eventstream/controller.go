package eventstream

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/client-go/dynamic"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/ttlcache"
)

const (
	GroupName = "events.socialweb.computer"
	Version   = "v1alpha1"

	FieldManager = "eventstream-controller"

	DefaultRetryAfter = 2 * time.Second

	pdsCacheTTL = 2 * time.Minute
)

var EventStreamGVR = eventsv1alpha1.EventStreamGVR

var EventSourceGVR = eventsv1alpha1.EventSourceGVR

var RecordWatchGVR = v1alpha1.AtprotoRecordWatchGVR

var AtprotoRepoGVR = v1alpha1.AtprotoRepoGVR

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Workspace string
	Dynamic   DynamicFactory
	Resolver  *pdsclient.Resolver
	Log       *slog.Logger

	Directory       identity.Directory
	PLCDirectoryURL string
	HTTPClient      *http.Client

	RetryAfter time.Duration

	FlushDelay time.Duration
}

type Controller struct {
	opts Options
	log  *slog.Logger

	directory identity.Directory
	http      *http.Client

	pdsCache *ttlcache.Cache[syntax.DID, pdsclient.PDS]
	verifyAt *ttlcache.Cache[string, struct{}]

	mu    sync.Mutex
	sinks map[string]*watchSink
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
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	opts.RetryAfter = opt.Positive(opts.RetryAfter, DefaultRetryAfter)
	httpClient := opts.HTTPClient
	if httpClient == nil && opts.Resolver.Handlers != nil {
		httpClient = opts.Resolver.Handlers.HTTPClient()
	}
	httpClient = httpClientWithFallback(httpClient)
	directory := opts.Directory
	if directory == nil {
		directory = newDirectory(httpClient, opts.PLCDirectoryURL, opts.Workspace)
	}
	return &Controller{
		opts:      opts,
		log:       opts.Log.With("component", "atprotorecordwatch-controller", "workspace", opts.Workspace),
		directory: directory,
		http:      httpClient,
		pdsCache:  ttlcache.New[syntax.DID, pdsclient.PDS](ttlcache.Options{TTL: pdsCacheTTL}),
		verifyAt:  ttlcache.New[string, struct{}](ttlcache.Options{TTL: watchVerifyInterval}),
		sinks:     map[string]*watchSink{},
	}, nil
}

func newDirectory(httpClient *http.Client, plcURL, workspace string) identity.Directory {
	plcURL = cmp.Or(strings.TrimSpace(plcURL), "http://"+kcp.ServiceFQDN("plc", "default", workspace, kcp.DefaultServiceDomain))
	local := &identity.BaseDirectory{PLCURL: plcURL, HTTPClient: *httpClient, PLCClient: httpClient, SkipHandleVerification: true}
	public := &identity.BaseDirectory{PLCURL: identity.DefaultPLCURL, HTTPClient: *httpClient, PLCClient: httpClient, SkipHandleVerification: true}
	return fallbackDirectory{local, public}
}

func (c *Controller) Workspace() string {
	return c.opts.Workspace
}

func (c *Controller) Run(ctx context.Context) error {
	c.runRecordWatches(ctx)
	c.mu.Lock()
	sinks := make([]*watchSink, 0, len(c.sinks))
	for _, s := range c.sinks {
		sinks = append(sinks, s)
	}
	c.mu.Unlock()
	for _, s := range sinks {
		s.close()
	}
	return nil
}

package events

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecordwatch"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	eventstreamctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/jetstreameventsource"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/pdseventsource"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/relayeventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/delivery"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const CursorDir = "eventstreams"

type Options struct {
	Bus *bus.Bus

	Dynamic eventstream.DynamicFactory

	Resolver *pdsclient.Resolver

	PLCURL string

	StateDir string

	RecordWatchChildren atprotorecordwatch.Children

	RecordWatchRepos atprotorecordwatch.Repos

	RecordWatchValues atprotorecordwatch.ValueReader

	Scope func(workspace string) bool

	Log *slog.Logger
}

type sinks struct {
	opts Options

	mu sync.Mutex

	markets map[string]*eventstream.MarketSink
}

func Register(m *ctlruntime.Manager, opts Options) (func(context.Context) error, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	opts.Log = log
	backend := eventsource.BackendOptions{Bus: opts.Bus, Log: log}
	if _, err := eventsource.Register(m, eventsource.Options{Bus: opts.Bus, Log: log}); err != nil {
		return nil, err
	}
	if _, err := pdseventsource.Register(m, backend); err != nil {
		return nil, err
	}
	if _, err := relayeventsource.Register(m, backend); err != nil {
		return nil, err
	}
	if _, err := jetstreameventsource.Register(m, backend); err != nil {
		return nil, err
	}
	streams, err := eventstreamctl.Register(m, eventstreamctl.Options{Bus: opts.Bus, Log: log,
		Cursors: delivery.NewCursorStore(filepath.Join(opts.StateDir, CursorDir))})
	if err != nil {
		return nil, err
	}
	watches := eventstream.NewWatches(eventstream.WatchesConfig{PLCDirectoryURL: opts.PLCURL, HTTPClient: resolverHTTP(opts.Resolver), Log: log})
	if _, err := atprotorecordwatch.Register(m, atprotorecordwatch.Options{Bus: opts.Bus, Backend: watches, Children: opts.RecordWatchChildren,
		Repos: opts.RecordWatchRepos, Values: opts.RecordWatchValues, Scope: opts.Scope, Log: log}); err != nil {
		return nil, err
	}
	s := &sinks{opts: opts, markets: map[string]*eventstream.MarketSink{}}
	bus.Provide[contract.Sink](opts.Bus, schema.GroupKind{Group: eventsv1.GroupName, Kind: contract.HandlerKind}, contract.CapabilityDeliver, s.openMarket)
	return streams.Run, nil
}

func resolverHTTP(r *pdsclient.Resolver) *http.Client {
	switch {
	case r == nil:
		return nil
	case r.HTTPClient != nil:
		return r.HTTPClient
	case r.Handlers != nil:
		return r.Handlers.HTTPClient()
	}
	return nil
}

func (s *sinks) openMarket(_ context.Context, to commonv1.ObjectRef, _ []string) (contract.Sink, error) {
	role, ok := eventstream.MarketRole(to.Name)
	if !ok {
		return nil, fmt.Errorf("events: %s names no market role", to.String())
	}
	key := to.Workspace + "/" + to.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	if sink := s.markets[key]; sink != nil {
		return sink, nil
	}
	sink, err := eventstream.NewMarketSink(eventstream.MarketOptions{Workspace: to.Workspace, Role: role, Dynamic: s.opts.Dynamic,
		Resolver: s.opts.Resolver, PLCDirectoryURL: s.opts.PLCURL, Log: s.opts.Log})
	if err != nil {
		return nil, err
	}
	s.markets[key] = sink
	return sink, nil
}

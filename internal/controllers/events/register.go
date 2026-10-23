package events

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventsource"
	eventstreamctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/jetstreameventsource"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/pdseventsource"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/relayeventsource"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/events/delivery"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const CursorDir = "eventstreams"

type Options struct {
	Bus *bus.Bus

	Workspaces []string

	Dynamic eventstream.DynamicFactory

	Resolver *pdsclient.Resolver

	PLCURL string

	StateDir string

	Log *slog.Logger
}

type sinks struct {
	opts Options

	watches map[string]*eventstream.Controller

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
	s := &sinks{opts: opts, watches: map[string]*eventstream.Controller{}, markets: map[string]*eventstream.MarketSink{}}
	for _, workspace := range opts.Workspaces {
		workspace = strings.TrimSpace(workspace)
		if workspace == "" || s.watches[workspace] != nil {
			continue
		}
		ctl, err := eventstream.New(eventstream.Options{Workspace: workspace, Dynamic: opts.Dynamic, Resolver: opts.Resolver,
			PLCDirectoryURL: opts.PLCURL, Log: log})
		if err != nil {
			return nil, fmt.Errorf("events: the AtprotoRecordWatch controller of %s: %w", workspace, err)
		}
		s.watches[workspace] = ctl
	}
	bus.Provide[contract.Sink](opts.Bus, schema.GroupKind{Group: v1alpha1.GroupName, Kind: contract.RecordWatchKind}, contract.CapabilityDeliver, s.openWatch)
	bus.Provide[contract.Sink](opts.Bus, schema.GroupKind{Group: eventsv1.GroupName, Kind: contract.HandlerKind}, contract.CapabilityDeliver, s.openMarket)
	return func(ctx context.Context) error {
		g := supervise.New(ctx)
		g.Go("eventstreams", streams.Run)
		for workspace, ctl := range s.watches {
			g.Go("atprotorecordwatches@"+workspace, ctl.Run)
		}
		return g.Wait()
	}, nil
}

func (s *sinks) openWatch(_ context.Context, to commonv1.ObjectRef, _ []string) (contract.Sink, error) {
	ctl := s.watches[strings.TrimSpace(to.Workspace)]
	if ctl == nil {
		return nil, fmt.Errorf("events: no AtprotoRecordWatch controller serves the workspace %s", to.Workspace)
	}
	return ctl.Sink(to.Name), nil
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

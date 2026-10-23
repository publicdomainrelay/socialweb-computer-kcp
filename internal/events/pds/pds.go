package pds

import (
	"context"
	"errors"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
)

const SourcePrefix = "pds/"

var ErrEnded = errors.New("pds: the repo subscription ended")

type Open func(ctx context.Context, collections []string) (atprotorepo.Subscriber, error)

type Config struct {
	Repo string
}

type Deps struct {
	Open Open
}

type Source struct {
	repo string

	open Open
}

var (
	_ events.Source = (*Source)(nil)
	_ events.Stream = (*stream)(nil)
)

func New(cfg Config, deps Deps) (*Source, error) {
	if cfg.Repo == "" {
		return nil, errors.New("pds: a repo is required")
	}
	if deps.Open == nil {
		return nil, errors.New("pds: an Open function is required")
	}
	return &Source{repo: cfg.Repo, open: deps.Open}, nil
}

func (s *Source) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	sub, err := s.open(ctx, filter.Collections)
	if err != nil {
		return nil, err
	}
	out := &stream{commits: make(chan events.Event), done: make(chan struct{}), filter: filter, source: SourcePrefix + s.repo}
	out.group = supervise.New(ctx)
	out.group.Go("pds subscription "+s.repo, func(ctx context.Context) error {
		defer close(out.done)
		out.err = sub.Subscribe(ctx, atprotorepo.SubscribeOptions{Collections: filter.Collections, Cursor: filter.Cursor}, func(c atprotorepo.Commit) error {
			select {
			case out.commits <- FromCommit(c):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		return nil
	})
	return out, nil
}

type stream struct {
	group *supervise.Group

	commits chan events.Event

	done chan struct{}

	err error

	filter events.Filter

	source string
}

func (s *stream) Next(ctx context.Context) (events.Event, error) {
	select {
	case <-ctx.Done():
		return events.Event{}, ctx.Err()
	case e := <-s.commits:
		e.Source = s.source
		return s.filter.Narrow(e), nil
	case <-s.done:
		if s.err != nil {
			return events.Event{}, s.err
		}
		return events.Event{}, ErrEnded
	}
}

func (s *stream) Close() error {
	return s.group.Stop()
}

func FromCommit(c atprotorepo.Commit) events.Event {
	out := events.Event{Seq: c.Seq, Repo: c.Repo, Commit: c.CID, Rev: c.Rev, Time: c.Time}
	for _, op := range c.Ops {
		out.Ops = append(out.Ops, events.Op{Action: op.Action, Path: op.Collection + "/" + op.RKey, CID: op.CID})
	}
	return out
}

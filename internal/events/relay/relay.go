package relay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/wsconn"
)

const (
	subscribePath = "/xrpc/com.atproto.sync.subscribeRepos"

	commitType = "com.atproto.sync.subscribeRepos#commit"

	infoType = "com.atproto.sync.subscribeRepos#info"

	SourcePrefix = "relay/"
)

type link struct {
	Link string `json:"$link"`
}

type frameOp struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	CID    *link  `json:"cid"`
	Prev   *link  `json:"prev"`
}

type frame struct {
	Type    string    `json:"$type"`
	Seq     int64     `json:"seq"`
	Repo    string    `json:"repo"`
	Commit  *link     `json:"commit"`
	Rev     string    `json:"rev"`
	Ops     []frameOp `json:"ops"`
	Time    string    `json:"time"`
	Error   string    `json:"error"`
	Message string    `json:"message"`
	Name    string    `json:"name"`

	WantedCollections []string `json:"wantedCollections"`
	WantedDIDs        []string `json:"wantedDids"`
}

type Config struct {
	Base string
}

type Deps struct {
	Dial wsconn.DialFunc

	Log *slog.Logger
}

type Source struct {
	base string

	dial wsconn.DialFunc

	log *slog.Logger
}

var (
	_ events.Source = (*Source)(nil)
	_ events.Stream = (*stream)(nil)
)

func New(cfg Config, deps Deps) (*Source, error) {
	base, err := Base(cfg.Base)
	if err != nil {
		return nil, err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Source{base: base, dial: deps.Dial, log: log}, nil
}

func (s *Source) Address() string {
	return s.base
}

func Base(raw string) (string, error) {
	parsed, err := wsconn.Parse(raw)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), subscribePath)
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

const futureCursor = "FutureCursor"

func (s *Source) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	parsed, err := wsconn.Parse(s.base)
	if err != nil {
		return nil, err
	}
	wsconn.WebSocketScheme(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/") + subscribePath
	parsed.RawPath, parsed.Fragment, parsed.RawQuery = "", "", ""
	endpoint := parsed.String()
	collections, dids := filter.ServerParams()
	query := url.Values{}
	for _, c := range collections {
		query.Add(events.WantedCollectionsParam, c)
	}
	for _, d := range dids {
		query.Add(events.WantedDIDsParam, d)
	}
	var cursor any
	if filter.Cursor != nil {
		query.Set("cursor", strconv.FormatInt(*filter.Cursor, 10))
		cursor = *filter.Cursor
	}
	parsed.RawQuery = query.Encode()
	s.log.Info("relay_subscribe_dial", "url", endpoint, events.WantedCollectionsParam, collections,
		"wantedDidsCount", len(dids), events.WantedDIDsParam, events.LogDIDs(dids), "cursor", cursor)
	conn, err := wsconn.Dial(ctx, parsed.String(), s.dial)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	out := &stream{conn: conn, filter: filter, source: SourcePrefix + s.base}
	if len(collections)+len(dids) > 0 {
		out.requested = events.ServerFilter{Collections: collections, DIDs: dids}
		out.announce = &events.ServerFilter{Collections: collections, DIDs: dids, Supported: events.ServerFilterUnknown}
		out.probing = true
	}
	return out, nil
}

type stream struct {
	conn *wsconn.Conn

	filter events.Filter

	source string

	requested events.ServerFilter

	announce *events.ServerFilter

	probing bool

	pending *events.Event
}

func (s *stream) Next(ctx context.Context) (events.Event, error) {
	if a := s.announce; a != nil {
		s.announce = nil
		return events.Event{Source: s.source, ServerFilter: a}, nil
	}
	if p := s.pending; p != nil {
		s.pending = nil
		return *p, nil
	}
	for {
		data, err := s.conn.Read(ctx)
		if err != nil {
			return events.Event{}, err
		}
		event, ok, err := decode(data)
		if err != nil {
			return events.Event{}, err
		}
		event.Source = s.source
		if s.probing {
			s.probing = false
			if ok && event.ServerFilter != nil {
				return event, nil
			}
			result := s.requested
			result.Supported = events.ServerFilterUnsupported
			if ok {
				narrowed := s.filter.Narrow(event)
				s.pending = &narrowed
			}
			return events.Event{Source: s.source, ServerFilter: &result}, nil
		}
		if !ok || event.ServerFilter != nil {
			continue
		}
		return s.filter.Narrow(event), nil
	}
}

func (s *stream) Close() error {
	return s.conn.Close()
}

func decode(data []byte) (events.Event, bool, error) {
	if len(data) > 0 && data[0] != '{' {
		return decodeCBOR(data)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return events.Event{}, false, fmt.Errorf("relay: decoding a frame: %w", err)
	}
	if f.Error == futureCursor {
		return events.Event{}, false, fmt.Errorf("relay: %w: %s", events.ErrFutureCursor, strings.TrimSpace(f.Message))
	}
	if f.Error != "" {
		return events.Event{}, false, fmt.Errorf("relay: %s: %s", f.Error, cmp.Or(strings.TrimSpace(f.Message), "relay stream error"))
	}
	if f.Type == infoType && f.Name == events.ServerFilterInfoName {
		return events.Event{ServerFilter: &events.ServerFilter{Collections: f.WantedCollections, DIDs: f.WantedDIDs,
			Supported: events.ServerFilterSupported}}, true, nil
	}
	if f.Type != "" && f.Type != commitType {
		return events.Event{}, false, nil
	}
	event, err := fromFrame(f)
	return event, err == nil, err
}

func fromFrame(f frame) (events.Event, error) {
	repo, err := syntax.ParseDID(strings.TrimSpace(f.Repo))
	if err != nil {
		return events.Event{}, fmt.Errorf("relay: a frame has an invalid repo: %w", err)
	}
	if f.Seq < 0 {
		return events.Event{}, errors.New("relay: a frame has a negative seq")
	}
	out := events.Event{Seq: f.Seq, Repo: repo.String(), Rev: strings.TrimSpace(f.Rev)}
	if f.Commit != nil {
		out.Commit = f.Commit.Link
	}
	for _, op := range f.Ops {
		converted := events.Op{Action: op.Action, Path: op.Path}
		if op.CID != nil {
			converted.CID = op.CID.Link
		}
		if op.Prev != nil {
			converted.Prev = op.Prev.Link
		}
		out.Ops = append(out.Ops, converted)
	}
	if raw := strings.TrimSpace(f.Time); raw != "" {
		if out.Time, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return events.Event{}, fmt.Errorf("relay: a frame has an invalid time: %w", err)
		}
	}
	return out, nil
}

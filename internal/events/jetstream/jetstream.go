package jetstream

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
	commitKind = "commit"

	SourcePrefix = "jetstream/"

	V1Path = "/subscribe"

	V2Path = "/xrpc/network.bsky.jetstream.subscribeEvents"

	v2CommitType = "network.bsky.jetstream.subscribeEvents#commit"

	v2MessageType = "message"

	v2ErrorType = "error"
)

type v2Frame struct {
	Type    string          `json:"$type"`
	Payload json.RawMessage `json:"payload"`
	Error   string          `json:"error"`
	Message string          `json:"message"`
}

type v2Commit struct {
	Type        string  `json:"$type"`
	Did         string  `json:"did"`
	Time        string  `json:"time"`
	WitnessedAt string  `json:"witnessedAt"`
	Rev         string  `json:"rev"`
	Operation   string  `json:"operation"`
	Collection  string  `json:"collection"`
	Rkey        string  `json:"rkey"`
	CID         *string `json:"cid"`
}

type frame struct {
	Did     string  `json:"did"`
	TimeUS  int64   `json:"time_us"`
	Kind    string  `json:"kind"`
	Commit  *commit `json:"commit"`
	Error   string  `json:"error"`
	Message string  `json:"message"`
}

type commit struct {
	Rev        string  `json:"rev"`
	Operation  string  `json:"operation"`
	Collection string  `json:"collection"`
	Rkey       string  `json:"rkey"`
	CID        *string `json:"cid"`
}

type Config struct {
	URL string
}

type Deps struct {
	Dial wsconn.DialFunc

	Log *slog.Logger
}

type Source struct {
	url string

	v2 bool

	dial wsconn.DialFunc

	log *slog.Logger
}

var (
	_ events.Source = (*Source)(nil)
	_ events.Stream = (*stream)(nil)
)

func New(cfg Config, deps Deps) (*Source, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("jetstream: a url is required")
	}
	endpoint, err := Endpoint(cfg.URL)
	if err != nil {
		return nil, err
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &Source{url: endpoint, v2: IsV2(parsed.Path), dial: deps.Dial, log: log}, nil
}

func (s *Source) Address() string {
	return s.url
}

func Endpoint(raw string) (string, error) {
	parsed, err := wsconn.Parse(raw)
	if err != nil {
		return "", err
	}
	parsed.Fragment = ""
	if strings.Trim(parsed.Path, "/") == "" {
		parsed.Path, parsed.RawPath = V2Path, ""
	}
	return parsed.String(), nil
}

func IsV2(path string) bool {
	return strings.HasSuffix(strings.TrimRight(path, "/"), V2Path)
}

func (s *Source) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	parsed, err := wsconn.Parse(s.url)
	if err != nil {
		return nil, err
	}
	wsconn.WebSocketScheme(parsed)
	collections, dids := filter.ServerParams()
	collectionsParam, didsParam := events.WantedCollectionsParam, events.WantedDIDsParam
	query := parsed.Query()
	if s.v2 {
		collectionsParam, didsParam = "collections", "dids"
		query.Set("kinds", commitKind)
	} else {
		query.Set("compress", "false")
	}
	for _, c := range collections {
		query.Add(collectionsParam, c)
	}
	for _, d := range dids {
		query.Add(didsParam, d)
	}
	var cursor any
	if filter.Cursor != nil && *filter.Cursor > 0 {
		query.Set("cursor", strconv.FormatInt(*filter.Cursor, 10))
		cursor = *filter.Cursor
	}
	endpoint := *parsed
	endpoint.RawQuery = ""
	parsed.RawQuery = query.Encode()
	s.log.Info("jetstream_subscribe_dial", "url", endpoint.String(), "v2", s.v2, collectionsParam, collections,
		didsParam+"Count", len(dids), didsParam, events.LogDIDs(dids), "cursor", cursor)
	conn, err := wsconn.Dial(ctx, parsed.String(), s.dial)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	out := &stream{conn: conn, filter: filter, source: SourcePrefix + s.url, v2: s.v2}
	if len(collections)+len(dids) > 0 {
		out.announce = &events.ServerFilter{Collections: collections, DIDs: dids, Supported: events.ServerFilterSupported}
	}
	return out, nil
}

type stream struct {
	conn *wsconn.Conn

	filter events.Filter

	source string

	v2 bool

	announce *events.ServerFilter
}

func (s *stream) Next(ctx context.Context) (events.Event, error) {
	if a := s.announce; a != nil {
		s.announce = nil
		return events.Event{Source: s.source, ServerFilter: a}, nil
	}
	for {
		data, err := s.conn.Read(ctx)
		if err != nil {
			return events.Event{}, err
		}
		decode := decodeV1
		if s.v2 {
			decode = decodeV2
		}
		event, ok, err := decode(data)
		if err != nil {
			return events.Event{}, err
		}
		if !ok {
			continue
		}
		event.Source = s.source
		return s.filter.Narrow(event), nil
	}
}

func decodeV1(data []byte) (events.Event, bool, error) {
	var f frame
	if err := json.Unmarshal(data, &f); err != nil {
		return events.Event{}, false, fmt.Errorf("jetstream: decoding a frame: %w", err)
	}
	if f.Error != "" {
		return events.Event{}, false, fmt.Errorf("jetstream: %s: %s", f.Error, cmp.Or(strings.TrimSpace(f.Message), "jetstream error"))
	}
	if strings.ToLower(strings.TrimSpace(f.Kind)) != commitKind || f.Commit == nil {
		return events.Event{}, false, nil
	}
	event, err := fromFrame(f)
	return event, err == nil, err
}

func decodeV2(data []byte) (events.Event, bool, error) {
	var f v2Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return events.Event{}, false, fmt.Errorf("jetstream: decoding a frame: %w", err)
	}
	if f.Type == v2ErrorType || f.Error != "" {
		return events.Event{}, false, fmt.Errorf("jetstream: %s: %s", cmp.Or(f.Error, "StreamError"), cmp.Or(strings.TrimSpace(f.Message), "jetstream error"))
	}
	if f.Type != v2MessageType || len(f.Payload) == 0 {
		return events.Event{}, false, nil
	}
	var c v2Commit
	if err := json.Unmarshal(f.Payload, &c); err != nil {
		return events.Event{}, false, fmt.Errorf("jetstream: decoding a message: %w", err)
	}
	if c.Type != v2CommitType {
		return events.Event{}, false, nil
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(cmp.Or(c.WitnessedAt, c.Time)))
	if err != nil {
		return events.Event{}, false, fmt.Errorf("jetstream: a commit has an invalid time: %w", err)
	}
	event, err := fromFrame(frame{Did: c.Did, TimeUS: at.UnixMicro(), Kind: commitKind,
		Commit: &commit{Rev: c.Rev, Operation: c.Operation, Collection: c.Collection, Rkey: c.Rkey, CID: c.CID}})
	return event, err == nil, err
}

func (s *stream) Close() error {
	return s.conn.Close()
}

func fromFrame(f frame) (events.Event, error) {
	repo, err := syntax.ParseDID(strings.TrimSpace(f.Did))
	if err != nil {
		return events.Event{}, fmt.Errorf("jetstream: a frame has an invalid did: %w", err)
	}
	c := f.Commit
	collection, rkey := strings.TrimSpace(c.Collection), strings.TrimSpace(c.Rkey)
	if collection == "" || rkey == "" {
		return events.Event{}, errors.New("jetstream: a commit needs a collection and an rkey")
	}
	op := events.Op{Action: strings.ToLower(strings.TrimSpace(c.Operation)), Path: collection + "/" + rkey}
	if op.Action == "" {
		return events.Event{}, errors.New("jetstream: a commit needs an operation")
	}
	if c.CID != nil {
		op.CID = strings.TrimSpace(*c.CID)
	}
	out := events.Event{Seq: f.TimeUS, Repo: repo.String(), Rev: strings.TrimSpace(c.Rev), Ops: []events.Op{op}}
	if f.TimeUS > 0 {
		out.Time = time.UnixMicro(f.TimeUS).UTC()
	}
	return out, nil
}

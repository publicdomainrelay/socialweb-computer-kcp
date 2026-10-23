package jetstream

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

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
}

type Source struct {
	url string

	dial wsconn.DialFunc
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
	return &Source{url: endpoint, dial: deps.Dial}, nil
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
	return parsed.String(), nil
}

func (s *Source) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	parsed, err := wsconn.Parse(s.url)
	if err != nil {
		return nil, err
	}
	wsconn.WebSocketScheme(parsed)
	query := parsed.Query()
	query.Set("compress", "false")
	for _, collection := range filter.Collections {
		if collection = strings.TrimSpace(collection); collection != "" {
			query.Add("wantedCollections", collection)
		}
	}
	for _, did := range filter.DIDs {
		if did = strings.TrimSpace(did); did != "" {
			query.Add("wantedDids", did)
		}
	}
	if filter.Cursor != nil && *filter.Cursor > 0 {
		query.Set("cursor", strconv.FormatInt(*filter.Cursor, 10))
	}
	parsed.RawQuery = query.Encode()
	conn, err := wsconn.Dial(ctx, parsed.String(), s.dial)
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	return &stream{conn: conn, filter: filter, source: SourcePrefix + s.url}, nil
}

type stream struct {
	conn *wsconn.Conn

	filter events.Filter

	source string
}

func (s *stream) Next(ctx context.Context) (events.Event, error) {
	for {
		data, err := s.conn.Read(ctx)
		if err != nil {
			return events.Event{}, err
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			return events.Event{}, fmt.Errorf("jetstream: decoding a frame: %w", err)
		}
		if f.Error != "" {
			return events.Event{}, fmt.Errorf("jetstream: %s: %s", f.Error, cmp.Or(strings.TrimSpace(f.Message), "jetstream error"))
		}
		if strings.ToLower(strings.TrimSpace(f.Kind)) != commitKind || f.Commit == nil {
			continue
		}
		event, err := fromFrame(f)
		if err != nil {
			return events.Event{}, err
		}
		event.Source = s.source
		return s.filter.Narrow(event), nil
	}
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

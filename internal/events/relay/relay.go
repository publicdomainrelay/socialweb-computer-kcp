package relay

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

type Config struct {
	Base string
}

type Deps struct {
	Dial wsconn.DialFunc
}

type Source struct {
	base string

	dial wsconn.DialFunc
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
	return &Source{base: base, dial: deps.Dial}, nil
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

func (s *Source) Subscribe(ctx context.Context, filter events.Filter) (events.Stream, error) {
	parsed, err := wsconn.Parse(s.base)
	if err != nil {
		return nil, err
	}
	wsconn.WebSocketScheme(parsed)
	parsed.Path = strings.TrimRight(parsed.Path, "/") + subscribePath
	parsed.RawPath, parsed.Fragment, parsed.RawQuery = "", "", ""
	if filter.Cursor != nil {
		parsed.RawQuery = url.Values{"cursor": {strconv.FormatInt(*filter.Cursor, 10)}}.Encode()
	}
	conn, err := wsconn.Dial(ctx, parsed.String(), s.dial)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	return &stream{conn: conn, filter: filter, source: SourcePrefix + s.base}, nil
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
	if f.Error != "" {
		return events.Event{}, false, fmt.Errorf("relay: %s: %s", f.Error, cmp.Or(strings.TrimSpace(f.Message), "relay stream error"))
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

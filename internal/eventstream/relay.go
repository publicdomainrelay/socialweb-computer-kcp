package eventstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/gorilla/websocket"
	"github.com/ipfs/go-cid"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

const (
	relayHandshakeTimeout = 15 * time.Second
	relayReadTimeout      = 2 * time.Minute
	relayMaxFrameBytes    = 8 << 20
	relayCommitType       = "com.atproto.sync.subscribeRepos#commit"
)

type repoStream interface {
	Next(ctx context.Context) (pdsclient.RepoEvent, error)
	Close() error
}

type relayLink struct {
	Link string `json:"$link"`
}

type relayOp struct {
	Action string     `json:"action"`
	Path   string     `json:"path"`
	CID    *relayLink `json:"cid"`
	Prev   *relayLink `json:"prev"`
}

type relayFrame struct {
	Type     string      `json:"$type"`
	Seq      int64       `json:"seq"`
	Repo     string      `json:"repo"`
	Commit   *relayLink  `json:"commit"`
	Rev      string      `json:"rev"`
	Since    *string     `json:"since"`
	Blocks   any         `json:"blocks"`
	Ops      []relayOp   `json:"ops"`
	TooBig   bool        `json:"tooBig"`
	Blobs    []relayLink `json:"blobs"`
	PrevData *relayLink  `json:"prevData"`
	Time     string      `json:"time"`
	Error    string      `json:"error"`
	Message  string      `json:"message"`
}

type relayEventStream struct {
	conn *websocket.Conn
}

func relayBaseURL(ref eventsv1alpha1.EventStreamRef) (string, error) {
	kind := strings.ToLower(strings.TrimSpace(ref.Kind))
	if kind == "" {
		kind = strings.ToLower(eventsv1alpha1.EventStreamDefaultRelayKind)
	}
	name := strings.ToLower(strings.TrimSpace(ref.Name))
	if name == "" {
		return "", errors.New("eventstream: relayRef needs a name")
	}
	workspace := strings.TrimSpace(ref.Workspace)
	if workspace != "" {
		return "http://" + kcp.ServiceFQDN(kind, "default", workspace, kcp.DefaultServiceDomain), nil
	}
	return "http://" + kind + "." + name, nil
}

func openRelayStream(ctx context.Context, base string, cursor *int64, handlers *component.HandlerRegistry) (repoStream, error) {
	ctx = component.WithSystemCaller(ctx)
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("eventstream: relay URL %q: %w", base, err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "wss", "ws":
	default:
		parsed.Scheme = "ws"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/xrpc/com.atproto.sync.subscribeRepos"
	parsed.RawPath = ""
	parsed.Fragment = ""
	parsed.RawQuery = ""
	if cursor != nil {
		query := url.Values{}
		query.Set("cursor", strconv.FormatInt(*cursor, 10))
		parsed.RawQuery = query.Encode()
	}
	dialer := &websocket.Dialer{
		Proxy:            nil,
		HandshakeTimeout: relayHandshakeTimeout,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if handlers != nil {
		dialer.NetDialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
			return handlers.DialHTTP(ctx, address)
		}
	}
	conn, response, err := dialer.DialContext(ctx, parsed.String(), nil)
	if err != nil {
		if response != nil {
			message := http.StatusText(response.StatusCode)
			if message == "" {
				message = "websocket handshake failed"
			}
			return nil, fmt.Errorf("eventstream: relay websocket handshake: %s", message)
		}
		return nil, fmt.Errorf("eventstream: relay websocket dial: %w", err)
	}
	conn.SetReadLimit(relayMaxFrameBytes)
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	_ = conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
	return &relayEventStream{conn: conn}, nil
}

func (s *relayEventStream) Next(ctx context.Context) (pdsclient.RepoEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pdsclient.RepoEvent{}, err
	}
	_ = s.conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
	stop := context.AfterFunc(ctx, func() {
		_ = s.conn.SetReadDeadline(time.Now())
	})
	defer stop()
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return pdsclient.RepoEvent{}, ctxErr
			}
			return pdsclient.RepoEvent{}, err
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
		var frame relayFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: decoding a relay frame: %w", err)
		}
		if frame.Error != "" {
			message := strings.TrimSpace(frame.Message)
			if message == "" {
				message = "relay stream error"
			}
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay %s: %s", frame.Error, message)
		}
		if frame.Type != "" && frame.Type != relayCommitType {
			continue
		}
		event, err := relayEvent(frame)
		if err != nil {
			return pdsclient.RepoEvent{}, err
		}
		return event, nil
	}
}

func (s *relayEventStream) Close() error {
	return s.conn.Close()
}

func relayEvent(frame relayFrame) (pdsclient.RepoEvent, error) {
	repo, err := syntax.ParseDID(strings.TrimSpace(frame.Repo))
	if err != nil {
		return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid repo: %w", err)
	}
	if frame.Seq < 0 {
		return pdsclient.RepoEvent{}, errors.New("eventstream: relay frame has a negative seq")
	}
	var commit cid.Cid
	if frame.Commit != nil && frame.Commit.Link != "" {
		commit, err = cid.Decode(frame.Commit.Link)
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid commit: %w", err)
		}
	}
	var rev syntax.TID
	if raw := strings.TrimSpace(frame.Rev); raw != "" {
		rev, err = syntax.ParseTID(raw)
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid rev: %w", err)
		}
	}
	var since *syntax.TID
	if frame.Since != nil && strings.TrimSpace(*frame.Since) != "" {
		parsed, err := syntax.ParseTID(strings.TrimSpace(*frame.Since))
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid since: %w", err)
		}
		since = &parsed
	}
	ops := make([]pdsclient.RepoOp, 0, len(frame.Ops))
	for _, op := range frame.Ops {
		converted := pdsclient.RepoOp{Action: op.Action, Path: op.Path}
		if op.CID != nil && op.CID.Link != "" {
			parsed, err := cid.Decode(op.CID.Link)
			if err != nil {
				return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid op cid: %w", err)
			}
			converted.CID = &parsed
		}
		if op.Prev != nil && op.Prev.Link != "" {
			parsed, err := cid.Decode(op.Prev.Link)
			if err != nil {
				return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid prev cid: %w", err)
			}
			converted.Prev = &parsed
		}
		ops = append(ops, converted)
	}
	when := time.Time{}
	if raw := strings.TrimSpace(frame.Time); raw != "" {
		when, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: relay frame has an invalid time: %w", err)
		}
	}
	return pdsclient.RepoEvent{
		Seq:    frame.Seq,
		Repo:   repo,
		Commit: commit,
		Rev:    rev,
		Since:  since,
		Ops:    ops,
		Time:   when,
		TooBig: frame.TooBig,
	}, nil
}

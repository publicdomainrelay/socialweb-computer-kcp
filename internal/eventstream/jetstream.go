package eventstream

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/gorilla/websocket"
	"github.com/ipfs/go-cid"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const jetstreamCommitKind = "commit"

type jetstreamFrame struct {
	Did     string           `json:"did"`
	TimeUS  int64            `json:"time_us"`
	Kind    string           `json:"kind"`
	Commit  *jetstreamCommit `json:"commit"`
	Error   string           `json:"error"`
	Message string           `json:"message"`
}

type jetstreamCommit struct {
	Rev        string          `json:"rev"`
	Operation  string          `json:"operation"`
	Collection string          `json:"collection"`
	Rkey       string          `json:"rkey"`
	Record     json.RawMessage `json:"record"`
	CID        *string         `json:"cid"`
}

type jetstreamEventStream struct {
	conn *websocket.Conn
}

func openJetstreamStream(ctx context.Context, base string, cursor *int64, collections, dids []string, handlers *component.HandlerRegistry) (repoStream, error) {
	parsed, err := parseStreamURL(base)
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}
	query := parsed.Query()
	query.Set("compress", "false")
	for _, collection := range collections {
		if collection = strings.TrimSpace(collection); collection != "" {
			query.Add("wantedCollections", collection)
		}
	}
	for _, did := range dids {
		if did = strings.TrimSpace(did); did != "" {
			query.Add("wantedDids", did)
		}
	}
	if cursor != nil && *cursor > 0 {
		query.Set("cursor", strconv.FormatInt(*cursor, 10))
	}
	parsed.RawQuery = query.Encode()
	dialer := &websocket.Dialer{
		Proxy:            nil,
		HandshakeTimeout: relayHandshakeTimeout,
		TLSClientConfig:  &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if handlers != nil {
		dialer.NetDialContext = streamDialContext(handlers)
	}
	conn, response, err := dialer.DialContext(ctx, parsed.String(), nil)
	if err != nil {
		if response != nil {
			message := http.StatusText(response.StatusCode)
			message = cmp.Or(message, "jetstream websocket handshake failed")
			return nil, fmt.Errorf("eventstream: jetstream websocket handshake: %s", message)
		}
		return nil, fmt.Errorf("eventstream: jetstream websocket dial: %w", err)
	}
	conn.SetReadLimit(relayMaxFrameBytes)
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	_ = conn.SetReadDeadline(time.Now().Add(relayReadTimeout))
	return &jetstreamEventStream{conn: conn}, nil
}

func (s *jetstreamEventStream) Next(ctx context.Context) (pdsclient.RepoEvent, error) {
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
		var frame jetstreamFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: decoding a jetstream frame: %w", err)
		}
		if frame.Error != "" {
			message := strings.TrimSpace(frame.Message)
			message = cmp.Or(message, "jetstream error")
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: jetstream %s: %s", frame.Error, message)
		}
		if strings.ToLower(strings.TrimSpace(frame.Kind)) != jetstreamCommitKind || frame.Commit == nil {
			continue
		}
		event, err := jetstreamEvent(frame)
		if err != nil {
			return pdsclient.RepoEvent{}, err
		}
		return event, nil
	}
}

func (s *jetstreamEventStream) Close() error {
	return s.conn.Close()
}

func jetstreamEvent(frame jetstreamFrame) (pdsclient.RepoEvent, error) {
	repo, err := syntax.ParseDID(strings.TrimSpace(frame.Did))
	if err != nil {
		return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: jetstream frame has an invalid did: %w", err)
	}
	commit := frame.Commit
	collection := strings.TrimSpace(commit.Collection)
	rkey := strings.TrimSpace(commit.Rkey)
	if collection == "" || rkey == "" {
		return pdsclient.RepoEvent{}, errors.New("eventstream: jetstream commit needs a collection and an rkey")
	}
	op := pdsclient.RepoOp{
		Action: strings.ToLower(strings.TrimSpace(commit.Operation)),
		Path:   collection + "/" + rkey,
	}
	if op.Action == "" {
		return pdsclient.RepoEvent{}, errors.New("eventstream: jetstream commit needs an operation")
	}
	if commit.CID != nil && strings.TrimSpace(*commit.CID) != "" {
		parsed, err := cid.Decode(strings.TrimSpace(*commit.CID))
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: jetstream commit has an invalid cid: %w", err)
		}
		op.CID = &parsed
	}
	var rev syntax.TID
	if raw := strings.TrimSpace(commit.Rev); raw != "" {
		rev, err = syntax.ParseTID(raw)
		if err != nil {
			return pdsclient.RepoEvent{}, fmt.Errorf("eventstream: jetstream commit has an invalid rev: %w", err)
		}
	}
	when := time.Time{}
	if frame.TimeUS > 0 {
		when = time.UnixMicro(frame.TimeUS).UTC()
	}
	return pdsclient.RepoEvent{
		Seq:  frame.TimeUS,
		Repo: repo,
		Rev:  rev,
		Ops:  []pdsclient.RepoOp{op},
		Time: when,
	}, nil
}

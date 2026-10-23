package relay

import (
	"cmp"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/fxamacker/cbor/v2"
	"github.com/ipfs/go-cid"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
)

const cidTag = 42

type cborHeader struct {
	Op int64  `cbor:"op"`
	T  string `cbor:"t"`
}

type cborError struct {
	Error   string `cbor:"error"`
	Message string `cbor:"message"`
}

type cborOp struct {
	Action string    `cbor:"action"`
	Path   string    `cbor:"path"`
	CID    *cbor.Tag `cbor:"cid"`
	Prev   *cbor.Tag `cbor:"prev"`
}

type cborCommit struct {
	Seq    int64     `cbor:"seq"`
	Repo   string    `cbor:"repo"`
	Commit *cbor.Tag `cbor:"commit"`
	Rev    string    `cbor:"rev"`
	Ops    []cborOp  `cbor:"ops"`
	Time   string    `cbor:"time"`
}

func decodeCBOR(data []byte) (events.Event, bool, error) {
	var header cborHeader
	rest, err := cbor.UnmarshalFirst(data, &header)
	if err != nil {
		return events.Event{}, false, fmt.Errorf("relay: decoding a CBOR frame header: %w", err)
	}
	if header.Op < 0 {
		var body cborError
		_ = cbor.Unmarshal(rest, &body)
		return events.Event{}, false, fmt.Errorf("relay: %s: %s", cmp.Or(strings.TrimSpace(body.Error), "StreamError"),
			cmp.Or(strings.TrimSpace(body.Message), "stream error"))
	}
	if header.T != "" && header.T != "#commit" {
		return events.Event{}, false, nil
	}
	var body cborCommit
	if err := cbor.Unmarshal(rest, &body); err != nil {
		return events.Event{}, false, fmt.Errorf("relay: decoding a CBOR commit: %w", err)
	}
	repo, err := syntax.ParseDID(strings.TrimSpace(body.Repo))
	if err != nil {
		return events.Event{}, false, fmt.Errorf("relay: a CBOR commit has an invalid repo: %w", err)
	}
	if body.Seq < 0 {
		return events.Event{}, false, errors.New("relay: a CBOR commit has a negative seq")
	}
	out := events.Event{Seq: body.Seq, Repo: repo.String(), Rev: strings.TrimSpace(body.Rev)}
	if out.Commit, err = cidString(body.Commit); err != nil {
		return events.Event{}, false, err
	}
	for _, op := range body.Ops {
		converted := events.Op{Action: op.Action, Path: op.Path}
		if converted.CID, err = cidString(op.CID); err != nil {
			return events.Event{}, false, err
		}
		if converted.Prev, err = cidString(op.Prev); err != nil {
			return events.Event{}, false, err
		}
		out.Ops = append(out.Ops, converted)
	}
	if raw := strings.TrimSpace(body.Time); raw != "" {
		if out.Time, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return events.Event{}, false, fmt.Errorf("relay: a CBOR commit has an invalid time: %w", err)
		}
	}
	return out, true, nil
}

func cidString(tag *cbor.Tag) (string, error) {
	if tag == nil {
		return "", nil
	}
	raw, ok := tag.Content.([]byte)
	if tag.Number != cidTag || !ok || len(raw) < 2 || raw[0] != 0 {
		return "", errors.New("relay: a CBOR frame carries an invalid cid")
	}
	parsed, err := cid.Cast(raw[1:])
	if err != nil || !parsed.Defined() {
		return "", errors.New("relay: a CBOR frame carries an invalid cid")
	}
	return parsed.String(), nil
}

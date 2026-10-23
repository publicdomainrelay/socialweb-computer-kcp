package relay

import (
	"encoding/json"
	"errors"

	comatproto "github.com/bluesky-social/indigo/api/atproto"
	lexutil "github.com/bluesky-social/indigo/lex/util"
)

const (
	FutureCursorError = "FutureCursor"

	CommitFrameType = "com.atproto.sync.subscribeRepos#commit"
)

var ErrFutureCursor = errors.New("relay: host rejected future cursor")

type Link struct {
	Link string `json:"$link"`
}

type Op struct {
	Action string `json:"action"`
	Path   string `json:"path"`
	CID    *Link  `json:"cid"`
	Prev   *Link  `json:"prev"`
}

type Frame struct {
	Type     string          `json:"$type"`
	Seq      int64           `json:"seq"`
	Repo     string          `json:"repo"`
	Commit   *Link           `json:"commit"`
	Rev      string          `json:"rev"`
	Since    *string         `json:"since"`
	Blocks   json.RawMessage `json:"blocks"`
	Ops      []Op            `json:"ops"`
	TooBig   bool            `json:"tooBig"`
	Blobs    []Link          `json:"blobs"`
	PrevData *Link           `json:"prevData"`
	Time     string          `json:"time"`
}

func emptyBlocks() json.RawMessage {
	return json.RawMessage(`{}`)
}

func lexLink(l lexutil.LexLink) *Link {
	if !l.Defined() {
		return nil
	}
	return &Link{Link: l.String()}
}

func frameFromCommit(evt *comatproto.SyncSubscribeRepos_Commit) Frame {
	f := Frame{
		Type:     CommitFrameType,
		Repo:     evt.Repo,
		Commit:   lexLink(evt.Commit),
		Rev:      evt.Rev,
		Since:    evt.Since,
		Blocks:   emptyBlocks(),
		TooBig:   evt.TooBig,
		PrevData: lexLinkPtr(evt.PrevData),
		Ops:      make([]Op, 0, len(evt.Ops)),
		Blobs:    make([]Link, 0, len(evt.Blobs)),
		Time:     evt.Time,
	}
	for _, op := range evt.Ops {
		out := Op{Action: op.Action, Path: op.Path}
		if op.Cid != nil {
			out.CID = lexLink(*op.Cid)
		}
		if op.Prev != nil {
			out.Prev = lexLink(*op.Prev)
		}
		f.Ops = append(f.Ops, out)
	}
	for _, b := range evt.Blobs {
		if l := lexLink(b); l != nil {
			f.Blobs = append(f.Blobs, *l)
		}
	}
	return f
}

func lexLinkPtr(l *lexutil.LexLink) *Link {
	if l == nil {
		return nil
	}
	return lexLink(*l)
}

func splitRecordPath(path string) (string, bool) {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			if i == 0 {
				return "", false
			}
			return path[:i], true
		}
	}
	return "", false
}

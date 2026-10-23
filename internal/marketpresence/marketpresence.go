package marketpresence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
)

const (
	MaxPresenceName = 253

	ReasonFresh = "HeartbeatFresh"

	ReasonNoDID = "NoDID"

	ReasonNoHeartbeat = "NoHeartbeat"

	ReasonInvalid = "HeartbeatInvalid"

	ReasonMismatch = "HeartbeatMismatch"

	ReasonInFuture = "HeartbeatInFuture"
)

func PresenceName(bidder string) string {
	name := "bidder-" + bidder
	if len(name) <= MaxPresenceName {
		return name
	}
	sum := sha256.Sum256([]byte(bidder))
	return "bidder-" + hex.EncodeToString(sum[:])[:32]
}

type Config struct {
	Reader market.RecordGetter

	Directory identity.Directory

	HTTPClient market.HTTPDoer
}

type Query struct {
	DID string

	OfferingURI string

	PLCDirectoryURL string

	Now time.Time
}

type Observation struct {
	At time.Time

	Reason string
}

type Heartbeats struct {
	reader market.RecordGetter

	directory identity.Directory

	http market.HTTPDoer

	mu sync.Mutex

	readers map[string]market.RecordGetter
}

func New(cfg Config) *Heartbeats {
	return &Heartbeats{reader: cfg.Reader, directory: cfg.Directory, http: cfg.HTTPClient, readers: map[string]market.RecordGetter{}}
}

func (h *Heartbeats) Observe(ctx context.Context, q Query) Observation {
	did := strings.TrimSpace(q.DID)
	if did == "" {
		return Observation{Reason: ReasonNoDID}
	}
	rec, err := h.readerFor(strings.TrimSpace(q.PLCDirectoryURL)).GetRecord(ctx, did, market.HeartbeatNSID, market.HeartbeatRKey)
	if err != nil {
		return Observation{Reason: ReasonNoHeartbeat}
	}
	heartbeat, err := market.ParseHeartbeat(rec.Value)
	if err != nil {
		return Observation{Reason: ReasonInvalid}
	}
	if offering := strings.TrimSpace(q.OfferingURI); offering != "" && heartbeat.Offering != offering {
		return Observation{Reason: ReasonMismatch}
	}
	at, err := market.ParseTime(heartbeat.CreatedAt)
	if err != nil {
		return Observation{Reason: ReasonInvalid}
	}
	if at.After(q.Now.Add(market.HeartbeatMaxSkew)) {
		return Observation{Reason: ReasonInFuture}
	}
	return Observation{At: at, Reason: ReasonFresh}
}

func (h *Heartbeats) readerFor(plc string) market.RecordGetter {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reader != nil {
		return h.reader
	}
	if reader, ok := h.readers[plc]; ok {
		return reader
	}
	dir := h.directory
	if dir == nil {
		dir = ingress.NewDirectory(plc)
	}
	reader := market.NewHTTPReader(dir)
	if h.http != nil {
		reader.Client = h.http
	}
	h.readers[plc] = reader
	return reader
}

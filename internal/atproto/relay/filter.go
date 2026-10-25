package relay

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
)

const (
	InfoFrameType = "com.atproto.sync.subscribeRepos#info"

	wildcardSuffix = ".*"

	wildcardProbe = ".a"
)

type SubscribeFilter struct {
	Collections []string

	DIDs []string

	dids map[string]bool
}

type serverFilterInfo struct {
	Type              string   `json:"$type"`
	Name              string   `json:"name"`
	Message           string   `json:"message"`
	WantedCollections []string `json:"wantedCollections"`
	WantedDIDs        []string `json:"wantedDids"`
}

func ParseSubscribeFilter(query url.Values) (SubscribeFilter, error) {
	var f SubscribeFilter
	seen := map[string]bool{}
	for _, raw := range query[events.WantedCollectionsParam] {
		c := strings.TrimSpace(raw)
		if c == "" || seen[c] {
			continue
		}
		if err := validCollection(c); err != nil {
			return SubscribeFilter{}, err
		}
		seen[c] = true
		f.Collections = append(f.Collections, c)
	}
	if len(f.Collections) > events.MaxServerFilterCollections {
		return SubscribeFilter{}, fmt.Errorf("at most %d %s", events.MaxServerFilterCollections, events.WantedCollectionsParam)
	}
	for _, raw := range query[events.WantedDIDsParam] {
		d := strings.TrimSpace(raw)
		if d == "" || f.dids[d] {
			continue
		}
		if _, err := syntax.ParseDID(d); err != nil {
			return SubscribeFilter{}, fmt.Errorf("%s %q is not a DID", events.WantedDIDsParam, d)
		}
		if f.dids == nil {
			f.dids = map[string]bool{}
		}
		f.dids[d] = true
		f.DIDs = append(f.DIDs, d)
	}
	if len(f.DIDs) > events.MaxServerFilterDIDs {
		return SubscribeFilter{}, fmt.Errorf("at most %d %s", events.MaxServerFilterDIDs, events.WantedDIDsParam)
	}
	return f, nil
}

func validCollection(c string) error {
	probe := c
	if prefix, ok := strings.CutSuffix(c, wildcardSuffix); ok {
		probe = prefix + wildcardProbe
	}
	if _, err := syntax.ParseNSID(probe); err != nil {
		return fmt.Errorf("%s %q is not an NSID or an NSID prefix ending in .*", events.WantedCollectionsParam, c)
	}
	return nil
}

func (f SubscribeFilter) Active() bool {
	return len(f.Collections) > 0 || len(f.DIDs) > 0
}

func (f SubscribeFilter) Apply(frame Frame) (Frame, bool) {
	if len(f.dids) > 0 && !f.dids[frame.Repo] {
		return Frame{}, false
	}
	if frame.Type != CommitFrameType || len(f.Collections) == 0 {
		return frame, true
	}
	kept := make([]Op, 0, len(frame.Ops))
	for _, op := range frame.Ops {
		if collection, ok := splitRecordPath(op.Path); ok && events.MatchCollection(f.Collections, collection) {
			kept = append(kept, op)
		}
	}
	if len(kept) == 0 {
		return Frame{}, false
	}
	frame.Ops = kept
	return frame, true
}

func (f SubscribeFilter) info() serverFilterInfo {
	return serverFilterInfo{Type: InfoFrameType, Name: events.ServerFilterInfoName,
		Message:           fmt.Sprintf("%s=%d %s=%d", events.WantedCollectionsParam, len(f.Collections), events.WantedDIDsParam, len(f.DIDs)),
		WantedCollections: append([]string{}, f.Collections...), WantedDIDs: append([]string{}, f.DIDs...)}
}

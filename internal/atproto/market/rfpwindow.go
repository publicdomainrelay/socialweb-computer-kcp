package market

import (
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const RFPBidWindow = 10 * time.Minute

func RFPOpenedAt(uri string, record map[string]any) (time.Time, bool) {
	if raw, ok := record["createdAt"].(string); ok {
		if at, err := syntax.ParseDatetimeTime(strings.TrimSpace(raw)); err == nil {
			return at, true
		}
	}
	tid, err := syntax.ParseTID(string(syntax.ATURI(uri).RecordKey()))
	if err != nil {
		return time.Time{}, false
	}
	return tid.Time(), true
}

func RFPBidWindowOver(uri string, record map[string]any, now time.Time) bool {
	opened, ok := RFPOpenedAt(uri, record)
	return ok && !now.Before(opened.Add(RFPBidWindow))
}

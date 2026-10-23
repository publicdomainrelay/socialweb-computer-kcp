package eventstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	RecordWatchLabel = "events.socialweb.computer/atprotorecordwatch"

	RecordWatchConditionReady       = "Ready"
	RecordWatchConditionProgressing = "Progressing"
	RecordWatchConditionFailed      = "Failed"
	RecordWatchConditionVerified    = v1alpha1.ConditionVerified

	EventStreamNamePrefix = "watch-"
	maxEventStreamName    = 253
)

func EventStreamNameForWatch(name string) string {
	candidate := EventStreamNamePrefix + name
	if len(candidate) <= maxEventStreamName {
		return candidate
	}
	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:4])
	keep := maxEventStreamName - len(suffix) - 1
	return candidate[:keep] + "-" + suffix
}

func StreamConditions(plans []SourcePlan, streams map[string]eventsapi.EventStream, generation int64) []metav1.Condition {
	var latestFailed *metav1.Condition
	readyCount := 0
	starting := false
	for _, plan := range plans {
		stream, ok := streams[plan.Name]
		if !ok {
			starting = true
			continue
		}
		if failed := meta.FindStatusCondition(stream.Status.Conditions, eventsapi.EventStreamConditionFailed); failed != nil && failed.Status == metav1.ConditionTrue {
			failedCopy := *failed
			latestFailed = &failedCopy
			continue
		}
		if ready := meta.FindStatusCondition(stream.Status.Conditions, eventsapi.EventStreamConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
			readyCount++
			continue
		}
		starting = true
	}
	if latestFailed != nil {
		return []metav1.Condition{
			conds.New(RecordWatchConditionReady, false, "StreamFailed", latestFailed.Message, generation),
			conds.New(RecordWatchConditionProgressing, false, "StreamFailed", latestFailed.Message, generation),
			conds.New(RecordWatchConditionFailed, true, "StreamFailed", latestFailed.Message, generation),
		}
	}
	if readyCount == len(plans) && !starting {
		return []metav1.Condition{
			conds.New(RecordWatchConditionReady, true, "StreamReady", "every EventStream is open", generation),
			conds.New(RecordWatchConditionProgressing, false, "StreamReady", "every EventStream is open", generation),
			conds.New(RecordWatchConditionFailed, false, "NoError", "every EventStream is open", generation),
		}
	}
	return []metav1.Condition{
		conds.New(RecordWatchConditionReady, false, "Starting", "the EventStreams are starting", generation),
		conds.New(RecordWatchConditionProgressing, true, "Starting", "the EventStreams are starting", generation),
		conds.New(RecordWatchConditionFailed, false, "NoError", "the EventStreams are starting", generation),
	}
}

type Verification struct {
	Records []v1alpha1.WatchedRecord

	Problems []string
}

func (v Verification) Verified() bool {
	return len(v.Problems) == 0
}

func (v Verification) Message() string {
	if len(v.Problems) > 0 {
		return "the watched records could not be verified: " + strings.Join(v.Problems, "; ")
	}
	return fmt.Sprintf("%d watched record(s) verified from their author PDS", len(v.Records))
}

func (w *Watches) Verify(ctx context.Context, records []v1alpha1.WatchedRecord, matches []Match) Verification {
	out := Verification{Records: make([]v1alpha1.WatchedRecord, 0, len(records))}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, record := range records {
		uri, err := syntax.ParseATURI(strings.TrimSpace(record.URI))
		if err != nil {
			continue
		}
		did, err := syntax.ParseDID(strings.TrimSpace(record.Did))
		if err != nil || uri.Authority().String() != did.String() {
			continue
		}
		pds, err := w.authorPDS(ctx, did)
		if err != nil {
			out.Problems = append(out.Problems, record.URI+": "+err.Error())
			out.Records = append(out.Records, record)
			continue
		}
		fetched, err := pds.GetRecord(ctx, pdsclient.RepoPath{Repo: did, Collection: uri.Collection(), Rkey: uri.RecordKey()}, nil)
		if err != nil {
			var xe *pdsclient.XRPCError
			if errors.As(err, &xe) && (xe.Code == "RecordNotFound" || xe.Code == "RepoNotFound" || xe.StatusCode == http.StatusNotFound) {
				continue
			}
			out.Problems = append(out.Problems, record.URI+": "+err.Error())
			out.Records = append(out.Records, record)
			continue
		}
		if fetched.URI.String() != record.URI || !MatchesRecord(fetched.Value, matches) {
			continue
		}
		if record.Cid == fetched.CID.String() && bytes.Equal(record.Value, fetched.Value) {
			out.Records = append(out.Records, record)
			continue
		}
		record.Cid = fetched.CID.String()
		record.Value = fetched.Value
		record.VerifiedAt = now
		out.Records = append(out.Records, record)
	}
	return out
}

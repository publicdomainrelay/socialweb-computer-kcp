package compose

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	RequesterStreamRole = "requester"

	bidderSourceLabel = "socialweb.computer/bidder-source"

	bidderSourcePrefix = "bidder-pds-"

	BidderStreamTTL = 30 * time.Minute

	MaxBidderStreams = 16

	MaxBidderStreamDIDs = 64
)

type bidderStream struct {
	source string

	pds string

	dids []string

	active bool

	seen time.Time
}

func BidderSourceName(pds string) string {
	sum := sha256.Sum256([]byte(pds))
	return bidderSourcePrefix + hex.EncodeToString(sum[:6])
}

func pdsOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host)
}

func contractActive(phase v1alpha1.ComputeContractPhase) bool {
	return phase != v1alpha1.ComputeContractReleased && phase != v1alpha1.ComputeContractFailed
}

func (c *Composer) bidderStreams(ctx context.Context, ws string, now time.Time) ([]bidderStream, time.Duration, error) {
	ri, err := c.Client(ws, ComputeContractGVR)
	if err != nil {
		return nil, 0, err
	}
	contracts, err := kube.List[v1alpha1.ComputeContract](ctx, ri, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	byPDS := map[string]*bidderStream{}
	for _, contract := range contracts {
		if contract.DeletionTimestamp != nil {
			continue
		}
		active := contractActive(contract.Status.Phase)
		for _, repo := range contract.Status.BidderRepos {
			did, pds := strings.TrimSpace(repo.Did), pdsOrigin(repo.Pds)
			if did == "" || pds == "" {
				continue
			}
			seen, _ := time.Parse(time.RFC3339, repo.SeenAt)
			if !active && now.Sub(seen) >= BidderStreamTTL {
				continue
			}
			s, ok := byPDS[pds]
			if !ok {
				s = &bidderStream{source: BidderSourceName(pds), pds: pds}
				byPDS[pds] = s
			}
			s.active = s.active || active
			if seen.After(s.seen) {
				s.seen = seen
			}
			if !slices.Contains(s.dids, did) && len(s.dids) < MaxBidderStreamDIDs {
				s.dids = append(s.dids, did)
			}
		}
	}
	out := make([]bidderStream, 0, len(byPDS))
	for _, s := range byPDS {
		slices.Sort(s.dids)
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b bidderStream) int {
		if a.active != b.active {
			if a.active {
				return -1
			}
			return 1
		}
		if !a.seen.Equal(b.seen) {
			return b.seen.Compare(a.seen)
		}
		return strings.Compare(a.pds, b.pds)
	})
	if len(out) > MaxBidderStreams {
		out = out[:MaxBidderStreams]
	}
	var next time.Duration
	for _, s := range out {
		if s.active {
			continue
		}
		if left := s.seen.Add(BidderStreamTTL).Sub(now); left > 0 && (next == 0 || left < next) {
			next = left
		}
	}
	return out, next, nil
}

func (c *Composer) applyBidderSources(ctx context.Context, r Root, ws, role string, streams []bidderStream) ([]commonv1.ObjectRef, error) {
	labels := map[string]string{bidderSourceLabel: role}
	keep := map[string]bool{}
	var refs []commonv1.ObjectRef
	for _, s := range streams {
		keep[s.source] = true
		backend := &eventsv1alpha1.RelayEventSource{ObjectMeta: metav1.ObjectMeta{Name: s.source, Labels: labels},
			Spec: eventsv1alpha1.RelayEventSourceSpec{URL: s.pds}}
		if err := c.ApplyTyped(ctx, r, ws, eventsv1alpha1.RelayEventSourceGVR, eventsv1alpha1.GroupVersion.String(), eventsv1alpha1.RelayEventSourceKind, backend); err != nil {
			return nil, err
		}
		backendRef := commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.RelayEventSourceKind, Name: s.source}
		abstract := &eventsv1alpha1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: s.source, Labels: labels},
			Spec: eventsv1alpha1.EventSourceSpec{BackendRef: backendRef}}
		if err := c.ApplyTyped(ctx, r, ws, eventsv1alpha1.EventSourceGVR, eventsv1alpha1.GroupVersion.String(), eventsv1alpha1.EventSourceKind, abstract); err != nil {
			return nil, err
		}
		source := EventSourceRef(s.source)
		source.Workspace = ws
		refs = append(refs, source)
	}
	selector := bidderSourceLabel + "=" + role
	if err := c.Prune(ctx, ws, eventsv1alpha1.EventSourceGVR, selector, keep); err != nil {
		return nil, err
	}
	if err := c.Prune(ctx, ws, eventsv1alpha1.RelayEventSourceGVR, selector, keep); err != nil {
		return nil, err
	}
	return refs, nil
}

func SoonerRequeue(res ctlruntime.Result, after time.Duration) ctlruntime.Result {
	if after > 0 && (res.RequeueAfter <= 0 || after < res.RequeueAfter) {
		res.RequeueAfter = after
	}
	return res
}

func (c *Composer) logBidderStreams(ws, role string, out []v1alpha1.BidderStreamStatus) {
	parts := make([]string, 0, len(out))
	for _, s := range out {
		parts = append(parts, s.EventStream+"="+s.Pds+"["+strings.Join(s.Dids, ",")+"]")
	}
	set := strings.Join(parts, " ")
	if last, ok := c.bidderSets.Load(ws + "|" + role); ok && last == set {
		return
	}
	c.bidderSets.Store(ws+"|"+role, set)
	c.log.Info("compose_bidder_streams", "workspace", ws, "role", role, "streams", len(out), "set", cmp.Or(set, "none"))
}

package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

const (
	marketStreamLabel = "socialweb.computer/market-stream"

	marketSourceLabel = "socialweb.computer/market-source"

	rootSourceValue = "root"

	NetworkRelaySourceName = "relay-network"

	LocalRepoSourceName = "local-repo"

	JetstreamEastSourceName = "jetstream-bsky-us-east"

	JetstreamWestSourceName = "jetstream-bsky-us-west"

	PublicJetstreamEastURL = "wss://jetstream2.us-east.bsky.network/subscribe"

	PublicJetstreamWestURL = "wss://jetstream1.us-west.bsky.network/subscribe"
)

var MarketStreamCollections = []string{
	market.RFPNSID,
	market.BidNSID,
	market.BidsFreeNSID,
	market.BidsX402NSID,
	market.AcceptNSID,
	market.AcceptsFreeNSID,
	market.AcceptsX402NSID,
	market.ReceiptNSID,
	market.ReceiptsFreeNSID,
	market.ReceiptsX402NSID,
	market.EventNSID,
	market.ComputeEventsVMDeleteNSID,
	market.ComputeEventsVMOnNetworkNSID,
	market.ComputeEventsVMStartedNSID,
	market.ComputeEventsVMTerminatedNSID,
	market.ComputeEventsVMHostKeysNSID,
	market.ComputeEventsVMRegisterIdentityNSID,
}

type SourceInput struct {
	Network *v1alpha1.MarketNetworkValue

	LocalDID string
}

type rootSource struct {
	Name string

	Relay *eventsv1alpha1.RelayEventSourceSpec

	Jetstream *eventsv1alpha1.JetstreamEventSourceSpec

	Pds *eventsv1alpha1.PdsEventSourceSpec
}

func (s rootSource) backend() (commonv1.ObjectRef, schema.GroupVersionResource, any) {
	ref := commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Name: s.Name}
	meta := metav1.ObjectMeta{Name: s.Name}
	switch {
	case s.Relay != nil:
		ref.Kind = eventsv1alpha1.RelayEventSourceKind
		return ref, eventsv1alpha1.RelayEventSourceGVR, &eventsv1alpha1.RelayEventSource{ObjectMeta: meta, Spec: *s.Relay}
	case s.Pds != nil:
		ref.Kind = eventsv1alpha1.PdsEventSourceKind
		return ref, eventsv1alpha1.PdsEventSourceGVR, &eventsv1alpha1.PdsEventSource{ObjectMeta: meta, Spec: *s.Pds}
	}
	ref.Kind = eventsv1alpha1.JetstreamEventSourceKind
	return ref, eventsv1alpha1.JetstreamEventSourceGVR, &eventsv1alpha1.JetstreamEventSource{ObjectMeta: meta, Spec: *s.Jetstream}
}

func MarketCommsMode(comms *v1alpha1.MarketComms) v1alpha1.MarketCommsMode {
	if comms == nil || strings.TrimSpace(string(comms.Mode)) == "" {
		return v1alpha1.MarketCommsModeXRPC
	}
	return comms.Mode
}

func OAuthReceiveOnly(mode v1alpha1.MarketIdentityMode) bool {
	switch mode {
	case v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthDelegated:
		return true
	}
	return false
}

func watchSources(network *v1alpha1.MarketNetworkValue) []rootSource {
	if network != nil {
		if relay := network.RelayURL(); relay != "" {
			return []rootSource{{Name: NetworkRelaySourceName, Relay: &eventsv1alpha1.RelayEventSourceSpec{URL: relay}}}
		}
	}
	return []rootSource{
		{Name: JetstreamEastSourceName, Jetstream: &eventsv1alpha1.JetstreamEventSourceSpec{URL: PublicJetstreamEastURL}},
		{Name: JetstreamWestSourceName, Jetstream: &eventsv1alpha1.JetstreamEventSourceSpec{URL: PublicJetstreamWestURL}},
	}
}

func rootSources(in SourceInput) []rootSource {
	out := watchSources(in.Network)
	if did := strings.TrimSpace(in.LocalDID); did != "" {
		out = append(out, rootSource{Name: LocalRepoSourceName, Pds: &eventsv1alpha1.PdsEventSourceSpec{
			RepoRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindAtprotoRepo, Name: atprotorepo.RepoName(did)}}})
	}
	return out
}

func WatchEventSources(in SourceInput) []commonv1.ObjectRef {
	var out []commonv1.ObjectRef
	for _, s := range watchSources(in.Network) {
		out = append(out, EventSourceRef(s.Name))
	}
	return out
}

func marketSourceRefs(ws string, comms *v1alpha1.MarketComms, network *v1alpha1.MarketNetworkValue, oauth bool) []commonv1.ObjectRef {
	var out []commonv1.ObjectRef
	add := func(ref commonv1.ObjectRef) {
		ref = commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.EventSourceKind, Name: strings.TrimSpace(ref.Name),
			Workspace: ref.InWorkspace(ws)}
		if ref.Name != "" && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	if comms != nil {
		for _, ref := range comms.EventSourceRefs {
			add(ref)
		}
	}
	if network != nil && network.RelayURL() != "" {
		add(EventSourceRef(NetworkRelaySourceName))
	}
	if len(out) == 0 && oauth {
		add(EventSourceRef(JetstreamEastSourceName))
	}
	return out
}

func marketStreamName(role string, ref commonv1.ObjectRef) string {
	sum := sha256.Sum256([]byte(role + "|" + ref.Workspace + "/" + ref.Name))
	return ObjectName("market-"+role, hex.EncodeToString(sum[:8]))
}

func eventLinkName(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "events-" + hex.EncodeToString(sum[:10])
}

func streamSelector(ws string, labels map[string]string, expressions []exposurev1alpha1.ComponentLinkSelectorRequirement) exposurev1alpha1.ComponentLinkSelector {
	return exposurev1alpha1.ComponentLinkSelector{Group: eventsv1alpha1.GroupName, Kind: "EventStream",
		Workspace: ws, MatchLabels: labels, MatchExpressions: expressions}
}

type Streams struct {
	Role string

	Workspace string

	Comms *v1alpha1.MarketComms

	Sources SourceInput

	ForceFirehose bool
}

func (c *Composer) ApplyMarketStreams(ctx context.Context, r Root, s Streams) ([]Link, error) {
	ws := workspaceOr(s.Workspace, r.Workspace)
	links, err := c.applyRootSources(ctx, r, ws, s.Sources)
	if err != nil {
		return nil, err
	}
	mode := MarketCommsMode(s.Comms)
	if s.ForceFirehose {
		mode = v1alpha1.MarketCommsModeFirehose
	}
	if mode != v1alpha1.MarketCommsModeFirehose && mode != v1alpha1.MarketCommsModeBoth {
		return links, c.Prune(ctx, ws, eventsv1alpha1.EventStreamGVR, marketStreamLabel+"="+s.Role, nil)
	}
	refs := marketSourceRefs(ws, s.Comms, s.Sources.Network, s.ForceFirehose)
	if len(refs) == 0 {
		return nil, fmt.Errorf("compose: comms mode %s names no event source (spec.comms.eventSourceRefs) and the network has no relay; it fails closed instead of using XRPC", mode)
	}
	streamLabels := map[string]string{marketStreamLabel: s.Role}
	sink := eventstream.MarketHandlerName(s.Role, ws)
	keep := map[string]bool{}
	for _, ref := range refs {
		name := marketStreamName(s.Role, ref)
		keep[name] = true
		sourceRef := ref
		if sourceRef.Workspace == ws {
			sourceRef.Workspace = ""
		}
		stream := &eventsv1alpha1.EventStream{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: streamLabels},
			Spec: eventsv1alpha1.EventStreamSpec{
				SourceRef: sourceRef,
				Filter:    eventsv1alpha1.EventStreamFilter{Collections: slices.Clone(MarketStreamCollections)},
				Sink:      eventsv1alpha1.EventStreamSink{Kind: eventsv1alpha1.EventStreamSinkHandler, Name: sink},
			}}
		if s.ForceFirehose {
			stream.Spec.Cursor = new(int64(0))
		}
		if err := c.ApplyTyped(ctx, r, ws, eventsv1alpha1.EventStreamGVR, eventsv1alpha1.GroupVersion.String(), "EventStream", stream); err != nil {
			return nil, err
		}
		user, err := c.userSourceLink(ctx, s.Role, ref)
		if err != nil {
			return nil, err
		}
		links = append(links, user...)
		links = append(links, SelectorLink(eventLinkName(s.Role, "subscribe", ref.Workspace, ref.Name), streamSelector(ws, streamLabels, nil), ref,
			Cap(events.CapabilitySubscribe, MarketStreamCollections...)))
	}
	links = append(links, SelectorLink(eventLinkName(s.Role, "deliver", ws, sink), streamSelector(ws, streamLabels, nil),
		commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: events.HandlerKind, Name: sink, Workspace: ws},
		Cap(events.CapabilityDeliver, sink)))
	if err := c.Prune(ctx, ws, eventsv1alpha1.EventStreamGVR, marketStreamLabel+"="+s.Role, keep); err != nil {
		return nil, err
	}
	return links, nil
}

func (c *Composer) applyRootSources(ctx context.Context, r Root, ws string, in SourceInput) ([]Link, error) {
	labels := map[string]string{marketSourceLabel: rootSourceValue}
	keep := map[string]bool{}
	var links []Link
	watchSelector := streamSelector(ws, nil, []exposurev1alpha1.ComponentLinkSelectorRequirement{
		{Key: eventstream.RecordWatchLabel, Operator: exposurev1alpha1.ComponentLinkSelectorExists}})
	for _, s := range rootSources(in) {
		keep[s.Name] = true
		backendRef, backendGVR, backend := s.backend()
		if err := c.ApplyTyped(ctx, r, ws, backendGVR, eventsv1alpha1.GroupVersion.String(), backendRef.Kind, withLabels(backend, labels)); err != nil {
			return nil, err
		}
		abstract := &eventsv1alpha1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Labels: labels},
			Spec: eventsv1alpha1.EventSourceSpec{BackendRef: backendRef}}
		if err := c.ApplyTyped(ctx, r, ws, eventsv1alpha1.EventSourceGVR, eventsv1alpha1.GroupVersion.String(), eventsv1alpha1.EventSourceKind, abstract); err != nil {
			return nil, err
		}
		source := EventSourceRef(s.Name)
		source.Workspace = ws
		backendRef.Workspace = ws
		links = append(links,
			SelectorLink(eventLinkName(rootSourceValue, "watch", ws, s.Name), watchSelector, source, Cap(events.CapabilitySubscribe, events.ScopeAll)),
			Link{Name: eventLinkName(rootSourceValue, "bind", ws, s.Name), Spec: exposurev1alpha1.ComponentLinkSpec{
				From: exposurev1alpha1.ComponentLinkFrom{Ref: &source}, To: backendRef,
				Capabilities: []exposurev1alpha1.LinkCapability{Cap(events.CapabilitySubscribe, events.ScopeAll)}}})
		switch {
		case s.Pds != nil:
			repo := s.Pds.RepoRef
			repo.Workspace = ws
			links = append(links, PdsEventSourceLink(backendRef, repo, ScopeAll))
		case s.Relay != nil && s.Relay.RelayRef != nil:
			links = append(links, RelayEventSourceLink(backendRef, AppRef(ComponentKindRelay, s.Relay.RelayRef.Name, ws)))
		}
	}
	links = append(links, SelectorLink(eventLinkName(rootSourceValue, "deliver", ws, AssociationWatch), watchSelector,
		commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: events.RecordWatchKind, Name: AssociationWatch, Workspace: ws},
		Cap(events.CapabilityDeliver, AssociationWatch)))
	selector := marketSourceLabel + "=" + rootSourceValue
	for _, gvr := range []schema.GroupVersionResource{eventsv1alpha1.EventSourceGVR, eventsv1alpha1.RelayEventSourceGVR,
		eventsv1alpha1.JetstreamEventSourceGVR, eventsv1alpha1.PdsEventSourceGVR} {
		if err := c.Prune(ctx, ws, gvr, selector, keep); err != nil {
			return nil, err
		}
	}
	return links, nil
}

func (c *Composer) userSourceLink(ctx context.Context, role string, ref commonv1.ObjectRef) ([]Link, error) {
	if ref.Name == NetworkRelaySourceName || ref.Name == LocalRepoSourceName || ref.Name == JetstreamEastSourceName || ref.Name == JetstreamWestSourceName {
		return nil, nil
	}
	source, err := Get[eventsv1alpha1.EventSource](ctx, c, ref.Workspace, eventsv1alpha1.EventSourceGVR, ref.Name)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	backend := source.Spec.BackendRef
	if backend.InWorkspace(ref.Workspace) != ref.Workspace {
		return nil, nil
	}
	backend.Workspace = ref.Workspace
	from := EventSourceRef(ref.Name)
	from.Workspace = ref.Workspace
	return []Link{{Name: eventLinkName(role, "bind", ref.Workspace, ref.Name), Spec: exposurev1alpha1.ComponentLinkSpec{
		From: exposurev1alpha1.ComponentLinkFrom{Ref: &from}, To: backend,
		Capabilities: []exposurev1alpha1.LinkCapability{Cap(events.CapabilitySubscribe, events.ScopeAll)}}}}, nil
}

func withLabels(obj any, labels map[string]string) any {
	switch o := obj.(type) {
	case *eventsv1alpha1.RelayEventSource:
		o.Labels = labels
	case *eventsv1alpha1.JetstreamEventSource:
		o.Labels = labels
	case *eventsv1alpha1.PdsEventSource:
		o.Labels = labels
	}
	return obj
}

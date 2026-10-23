package appctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/events"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

const (
	marketStreamLabel = "socialweb.computer/market-stream"

	marketSourceLabel = "socialweb.computer/market-source"

	marketLinkLabel = "socialweb.computer/market-link"

	rootSourceValue = "root"

	NetworkRelaySourceName = "relay-network"

	JetstreamEastSourceName = "jetstream-bsky-us-east"

	JetstreamWestSourceName = "jetstream-bsky-us-west"

	PublicJetstreamEastURL = "wss://jetstream2.us-east.bsky.network/subscribe"

	PublicJetstreamWestURL = "wss://jetstream1.us-west.bsky.network/subscribe"

	AssociationWatchName = "association"
)

var marketStreamCollections = []string{
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

type rootSource struct {
	Name string

	Relay *eventsv1alpha1.RelayEventSourceSpec

	Jetstream *eventsv1alpha1.JetstreamEventSourceSpec
}

func (s rootSource) backend() (commonv1.ObjectRef, schema.GroupVersionResource, any) {
	ref := commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Name: s.Name}
	meta := metav1.ObjectMeta{Name: s.Name}
	if s.Relay != nil {
		ref.Kind = eventsv1alpha1.RelayEventSourceKind
		return ref, eventsv1alpha1.RelayEventSourceGVR, &eventsv1alpha1.RelayEventSource{ObjectMeta: meta, Spec: *s.Relay}
	}
	ref.Kind = eventsv1alpha1.JetstreamEventSourceKind
	return ref, eventsv1alpha1.JetstreamEventSourceGVR, &eventsv1alpha1.JetstreamEventSource{ObjectMeta: meta, Spec: *s.Jetstream}
}

func marketCommsMode(comms *v1alpha1.MarketComms) v1alpha1.MarketCommsMode {
	if comms == nil || strings.TrimSpace(string(comms.Mode)) == "" {
		return v1alpha1.MarketCommsModeXRPC
	}
	return comms.Mode
}

func oauthReceiveOnly(mode v1alpha1.MarketIdentityMode) bool {
	switch mode {
	case v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthDelegated:
		return true
	}
	return false
}

func rootSources(network *v1alpha1.MarketNetworkValue) []rootSource {
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

func eventSourceRef(name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: eventsv1alpha1.EventSourceKind, Name: name}
}

func WatchEventSources(network *v1alpha1.MarketNetworkValue) []commonv1.ObjectRef {
	var out []commonv1.ObjectRef
	for _, s := range rootSources(network) {
		out = append(out, eventSourceRef(s.Name))
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
		add(eventSourceRef(NetworkRelaySourceName))
	}
	if len(out) == 0 && oauth {
		add(eventSourceRef(JetstreamEastSourceName))
	}
	return out
}

func marketStreamName(role string, ref commonv1.ObjectRef) string {
	sum := sha256.Sum256([]byte(role + "|" + ref.Workspace + "/" + ref.Name))
	return statusObjectName("market-"+role, hex.EncodeToString(sum[:8]))
}

func eventLinkName(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "events-" + hex.EncodeToString(sum[:10])
}

func (ctl *Controller) ensureMarketStreams(ctx context.Context, root *unstructured.Unstructured, ws, role string, comms *v1alpha1.MarketComms,
	network *v1alpha1.MarketNetworkValue, forceFirehose bool) error {
	if err := ctl.ensureRootSources(ctx, root, ws, network); err != nil {
		return err
	}
	mode := marketCommsMode(comms)
	if forceFirehose {
		mode = v1alpha1.MarketCommsModeFirehose
	}
	if mode != v1alpha1.MarketCommsModeFirehose && mode != v1alpha1.MarketCommsModeBoth {
		if err := ctl.pruneLabeled(ctx, ws, eventsv1alpha1.EventStreamGVR, marketStreamLabel+"="+role, nil); err != nil {
			return err
		}
		return ctl.pruneLabeled(ctx, ws, ComponentLinkGVR, marketLinkLabel+"="+role, nil)
	}
	refs := marketSourceRefs(ws, comms, network, forceFirehose)
	if len(refs) == 0 {
		return fmt.Errorf("appctl: comms mode %s names no event source (spec.comms.eventSourceRefs) and the network has no relay; it fails closed instead of using XRPC", mode)
	}
	streamLabels := map[string]string{marketStreamLabel: role}
	sink := eventstream.MarketHandlerName(role, ws)
	keepStreams, keepLinks := map[string]bool{}, map[string]bool{}
	for _, ref := range refs {
		name := marketStreamName(role, ref)
		keepStreams[name] = true
		sourceRef := ref
		if sourceRef.Workspace == ws {
			sourceRef.Workspace = ""
		}
		stream := &eventsv1alpha1.EventStream{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: streamLabels},
			Spec: eventsv1alpha1.EventStreamSpec{
				SourceRef: sourceRef,
				Filter:    eventsv1alpha1.EventStreamFilter{Collections: append([]string(nil), marketStreamCollections...)},
				Sink:      eventsv1alpha1.EventStreamSink{Kind: eventsv1alpha1.EventStreamSinkHandler, Name: sink},
			}}
		if forceFirehose {
			stream.Spec.Cursor = new(int64(0))
		}
		if err := ctl.applyEventObject(ctx, root, ws, eventsv1alpha1.EventStreamGVR, eventsv1alpha1.GroupVersion.String(), "EventStream", stream); err != nil {
			return err
		}
		if err := ctl.ensureUserSourceLink(ctx, root, ref, role, keepLinks); err != nil {
			return err
		}
		link := exposurev1alpha1.ComponentLinkSpec{
			From: streamSelector(ws, streamLabels, nil),
			To:   ref,
			Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilitySubscribe,
				Scope: append([]string(nil), marketStreamCollections...)}},
		}
		if err := ctl.applyEventLink(ctx, root, ref.Workspace, eventLinkName(role, "subscribe", ref.Workspace, ref.Name), role, link, keepLinks); err != nil {
			return err
		}
	}
	deliver := exposurev1alpha1.ComponentLinkSpec{
		From:         streamSelector(ws, streamLabels, nil),
		To:           commonv1.ObjectRef{Group: eventsv1alpha1.GroupName, Kind: events.HandlerKind, Name: sink},
		Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilityDeliver, Scope: []string{sink}}},
	}
	if err := ctl.applyEventLink(ctx, root, ws, eventLinkName(role, "deliver", ws, sink), role, deliver, keepLinks); err != nil {
		return err
	}
	if err := ctl.pruneLabeled(ctx, ws, eventsv1alpha1.EventStreamGVR, marketStreamLabel+"="+role, keepStreams); err != nil {
		return err
	}
	return ctl.pruneLabeled(ctx, ws, ComponentLinkGVR, marketLinkLabel+"="+role, keepLinks)
}

func (ctl *Controller) ensureRootSources(ctx context.Context, root *unstructured.Unstructured, ws string, network *v1alpha1.MarketNetworkValue) error {
	labels := map[string]string{marketSourceLabel: rootSourceValue}
	keepSources, keepLinks := map[string]bool{}, map[string]bool{}
	watchSelector := streamSelector(ws, nil, []exposurev1alpha1.ComponentLinkSelectorRequirement{
		{Key: eventstream.RecordWatchLabel, Operator: exposurev1alpha1.ComponentLinkSelectorExists}})
	for _, s := range rootSources(network) {
		keepSources[s.Name] = true
		backendRef, backendGVR, backend := s.backend()
		if err := ctl.applyEventObject(ctx, root, ws, backendGVR, eventsv1alpha1.GroupVersion.String(), backendRef.Kind, withLabels(backend, labels)); err != nil {
			return err
		}
		abstract := &eventsv1alpha1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Labels: labels},
			Spec: eventsv1alpha1.EventSourceSpec{BackendRef: backendRef}}
		if err := ctl.applyEventObject(ctx, root, ws, eventsv1alpha1.EventSourceGVR, eventsv1alpha1.GroupVersion.String(), eventsv1alpha1.EventSourceKind, abstract); err != nil {
			return err
		}
		source := eventSourceRef(s.Name)
		bind := exposurev1alpha1.ComponentLinkSpec{
			From:         exposurev1alpha1.ComponentLinkFrom{Ref: &source},
			To:           backendRef,
			Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilitySubscribe, Scope: []string{events.ScopeAll}}},
		}
		if err := ctl.applyEventLink(ctx, root, ws, eventLinkName(rootSourceValue, "bind", ws, s.Name), rootSourceValue, bind, keepLinks); err != nil {
			return err
		}
		watch := exposurev1alpha1.ComponentLinkSpec{
			From:         watchSelector,
			To:           source,
			Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilitySubscribe, Scope: []string{events.ScopeAll}}},
		}
		if err := ctl.applyEventLink(ctx, root, ws, eventLinkName(rootSourceValue, "watch", ws, s.Name), rootSourceValue, watch, keepLinks); err != nil {
			return err
		}
	}
	deliver := exposurev1alpha1.ComponentLinkSpec{
		From:         watchSelector,
		To:           commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: events.RecordWatchKind, Name: AssociationWatchName},
		Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilityDeliver, Scope: []string{AssociationWatchName}}},
	}
	if err := ctl.applyEventLink(ctx, root, ws, eventLinkName(rootSourceValue, "deliver", ws, AssociationWatchName), rootSourceValue, deliver, keepLinks); err != nil {
		return err
	}
	selector := marketSourceLabel + "=" + rootSourceValue
	for _, gvr := range []schema.GroupVersionResource{eventsv1alpha1.EventSourceGVR, eventsv1alpha1.RelayEventSourceGVR, eventsv1alpha1.JetstreamEventSourceGVR} {
		if err := ctl.pruneLabeled(ctx, ws, gvr, selector, keepSources); err != nil {
			return err
		}
	}
	return ctl.pruneLabeled(ctx, ws, ComponentLinkGVR, marketLinkLabel+"="+rootSourceValue, keepLinks)
}

func (ctl *Controller) ensureUserSourceLink(ctx context.Context, root *unstructured.Unstructured, ref commonv1.ObjectRef, role string, keep map[string]bool) error {
	if slices.ContainsFunc(rootSources(nil), func(s rootSource) bool { return s.Name == ref.Name }) || ref.Name == NetworkRelaySourceName {
		return nil
	}
	ri, err := ctl.client(ref.Workspace, eventsv1alpha1.EventSourceGVR)
	if err != nil {
		return err
	}
	u, err := ri.Get(ctx, ref.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var source eventsv1alpha1.EventSource
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &source); err != nil {
		return err
	}
	backend := source.Spec.BackendRef
	if backend.InWorkspace(ref.Workspace) != ref.Workspace {
		return nil
	}
	backend.Workspace = ""
	from := eventSourceRef(ref.Name)
	bind := exposurev1alpha1.ComponentLinkSpec{
		From:         exposurev1alpha1.ComponentLinkFrom{Ref: &from},
		To:           backend,
		Capabilities: []exposurev1alpha1.LinkCapability{{Name: events.CapabilitySubscribe, Scope: []string{events.ScopeAll}}},
	}
	return ctl.applyEventLink(ctx, root, ref.Workspace, eventLinkName(role, "bind", ref.Workspace, ref.Name), role, bind, keep)
}

func streamSelector(ws string, labels map[string]string, expressions []exposurev1alpha1.ComponentLinkSelectorRequirement) exposurev1alpha1.ComponentLinkFrom {
	return exposurev1alpha1.ComponentLinkFrom{Selector: &exposurev1alpha1.ComponentLinkSelector{Group: eventsv1alpha1.GroupName, Kind: "EventStream",
		Workspace: ws, MatchLabels: labels, MatchExpressions: expressions}}
}

func withLabels(obj any, labels map[string]string) any {
	switch o := obj.(type) {
	case *eventsv1alpha1.RelayEventSource:
		o.Labels = labels
	case *eventsv1alpha1.JetstreamEventSource:
		o.Labels = labels
	}
	return obj
}

func (ctl *Controller) applyEventLink(ctx context.Context, root *unstructured.Unstructured, ws, name, group string, spec exposurev1alpha1.ComponentLinkSpec, keep map[string]bool) error {
	keep[name] = true
	link := &exposurev1alpha1.ComponentLink{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{marketLinkLabel: group}}, Spec: spec}
	return ctl.applyEventObject(ctx, root, ws, ComponentLinkGVR, exposurev1alpha1.GroupVersion.String(), ComponentKindComponentLink, link)
}

func (ctl *Controller) applyEventObject(ctx context.Context, root *unstructured.Unstructured, ws string, gvr schema.GroupVersionResource, apiVersion, kind string, obj any) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	if root != nil && root.GetUID() != "" && ws == ctl.c.workspace() {
		u.SetOwnerReferences([]metav1.OwnerReference{ownerRefOf(root)})
	}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	ri, err := ctl.client(ws, gvr)
	if err != nil {
		return err
	}
	force := true
	if _, err := ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force}); err != nil {
		return fmt.Errorf("appctl: applying %s %s: %w", kind, u.GetName(), err)
	}
	return nil
}

func (ctl *Controller) pruneLabeled(ctx context.Context, ws string, gvr schema.GroupVersionResource, selector string, keep map[string]bool) error {
	ri, err := ctl.client(ws, gvr)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if keep[name] {
			continue
		}
		if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

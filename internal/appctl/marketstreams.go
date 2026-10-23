package appctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

const marketStreamLabel = "socialweb.computer/market-stream"

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

func marketTransportURLs(comms *v1alpha1.MarketComms, network *v1alpha1.MarketNetworkValue) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(values []string) {
		for _, raw := range values {
			raw = strings.TrimSpace(raw)
			if raw == "" || seen[raw] {
				continue
			}
			seen[raw] = true
			out = append(out, raw)
		}
	}
	if comms != nil {
		add(comms.RelayUrls)
		add(comms.FirehoseUrls)
	}
	if network != nil {
		add(network.RelayUrls)
		add(network.FirehoseUrls)
	}
	return out
}

func marketStreamName(role, url string) string {
	sum := sha256.Sum256([]byte(role + "|" + strings.TrimSpace(url)))
	return statusObjectName("market-"+role, hex.EncodeToString(sum[:8]))
}

func (ctl *Controller) ensureMarketStreams(ctx context.Context, ws, role string, comms *v1alpha1.MarketComms, network *v1alpha1.MarketNetworkValue, forceFirehose bool) error {
	mode := marketCommsMode(comms)
	if forceFirehose {
		mode = v1alpha1.MarketCommsModeFirehose
	}
	if mode != v1alpha1.MarketCommsModeFirehose && mode != v1alpha1.MarketCommsModeBoth {
		return ctl.deleteMarketStreams(ctx, ws, role)
	}
	urls := marketTransportURLs(comms, network)
	if len(urls) == 0 {
		return fmt.Errorf("appctl: comms mode %s has no relay or firehose URL; it fails closed instead of using XRPC", mode)
	}
	ri, err := ctl.client(ws, eventstream.EventStreamGVR)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, url := range urls {
		name := marketStreamName(role, url)
		keep[name] = true
		spec := eventsv1alpha1.EventStreamSpec{
			Source: eventsv1alpha1.EventStreamSource{
				Kind:        eventsv1alpha1.EventStreamSourceRelay,
				RelayUrls:   []string{url},
				Collections: append([]string(nil), marketStreamCollections...),
			},
			Sink: eventsv1alpha1.EventStreamSink{
				Kind: eventsv1alpha1.EventStreamSinkHandler,
				Name: eventstream.MarketHandlerName(role, ws),
			},
		}
		specMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
		if err != nil {
			return err
		}
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": eventsv1alpha1.GroupVersion.String(),
			"kind":       "EventStream",
			"metadata": map[string]any{
				"name":   name,
				"labels": map[string]any{marketStreamLabel: role},
			},
			"spec": specMap,
		}}
		existing, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := ri.Create(ctx, obj, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
			return err
		}
	}
	return ctl.deleteStaleMarketStreams(ctx, ri, role, keep)
}

func (ctl *Controller) deleteMarketStreams(ctx context.Context, ws, role string) error {
	ri, err := ctl.client(ws, eventstream.EventStreamGVR)
	if err != nil {
		return err
	}
	return ctl.deleteStaleMarketStreams(ctx, ri, role, nil)
}

func (ctl *Controller) deleteStaleMarketStreams(ctx context.Context, ri dynamic.ResourceInterface, role string, keep map[string]bool) error {
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: marketStreamLabel + "=" + role})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		item := &list.Items[i]
		if keep != nil && keep[item.GetName()] {
			continue
		}
		if err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

package compose

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	RequesterStreamRole = "requester"

	BidderStreamRole = "bidder"

	retiredBidderSourceLabel = "socialweb.computer/bidder-source"

	requesterSourcePrefix = "requesters-"

	requesterSourceValue = "requesters-"

	MaxRequesterStreamDIDs = 100
)

func RequesterSourceName(kind, raw string) string {
	host := raw
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil && u.Host != "" {
		host = u.Host
	}
	return ObjectName(requesterSourcePrefix+kind, host)
}

func liveComputeVM(vm *v1alpha1.ComputeVM) bool {
	if vm.DeletionTimestamp != nil || strings.TrimSpace(vm.Spec.Requester.Did) == "" {
		return false
	}
	return vm.Status.Phase != v1alpha1.ComputeVMPhaseLost && vm.Status.Phase != v1alpha1.ComputeVMPhaseFailed
}

func (c *Composer) liveRequesters(ctx context.Context, ws string) ([]string, error) {
	ri, err := c.Client(ws, v1alpha1.ComputeVMGVR)
	if err != nil {
		return nil, err
	}
	vms, err := kube.List[v1alpha1.ComputeVM](ctx, ri, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range vms {
		if did := strings.TrimSpace(vms[i].Spec.Requester.Did); liveComputeVM(&vms[i]) && !slices.Contains(out, did) {
			out = append(out, did)
		}
	}
	slices.Sort(out)
	if len(out) > MaxRequesterStreamDIDs {
		out = out[:MaxRequesterStreamDIDs]
	}
	return out, nil
}

func requesterSources(comms *v1alpha1.MarketComms, network *v1alpha1.MarketNetworkValue, requesters []string) []rootSource {
	if len(requesters) == 0 {
		return nil
	}
	relays := comms.FirehoseRelays(network)
	if relay := network.RelayURL(); relay != "" && (comms == nil || comms.RelayURLs == nil) {
		relays = []string{relay}
	}
	var jetstreams []string
	switch {
	case comms != nil && comms.JetstreamURLs != nil:
		jetstreams = comms.FirehoseJetstreams()
	case network.RelayURL() == "":
		jetstreams = []string{PublicJetstreamEastURL}
	}
	var out []rootSource
	for _, u := range relays {
		out = append(out, rootSource{Name: RequesterSourceName("relay", u), Relay: &eventsv1alpha1.RelayEventSourceSpec{URL: u}})
	}
	for _, u := range jetstreams {
		out = append(out, rootSource{Name: RequesterSourceName("jetstream", u), Jetstream: &eventsv1alpha1.JetstreamEventSourceSpec{URL: u}})
	}
	return out
}

func (c *Composer) pruneRetiredBidderSources(ctx context.Context, ws, role string) error {
	selector := retiredBidderSourceLabel + "=" + role
	if err := c.Prune(ctx, ws, eventsv1alpha1.EventSourceGVR, selector, nil); err != nil {
		return err
	}
	return c.Prune(ctx, ws, eventsv1alpha1.RelayEventSourceGVR, selector, nil)
}

func (c *Composer) logRequesterStreams(ws, role string, dids []string, names []string) {
	set := strings.Join(names, ",") + "[" + strings.Join(dids, ",") + "]"
	if len(names) == 0 {
		set = ""
	}
	if last, ok := c.requesterSets.Load(ws + "|" + role); ok && last == set {
		return
	}
	c.requesterSets.Store(ws+"|"+role, set)
	c.log.Info("compose_requester_streams", "workspace", ws, "role", role, "streams", len(names), "requesters", len(dids), "set", cmp.Or(set, "none"))
}

type requesterIndex struct {
	mu sync.Mutex

	byWorkspace map[string]map[string]string
}

func (x *requesterIndex) note(workspace, name, did string) bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.byWorkspace == nil {
		x.byWorkspace = map[string]map[string]string{}
	}
	vms := x.byWorkspace[workspace]
	if vms == nil {
		vms = map[string]string{}
		x.byWorkspace[workspace] = vms
	}
	prior, known := vms[name]
	if prior == did && (known || did == "") {
		return false
	}
	count := func(d string) int {
		n := 0
		for _, v := range vms {
			if v == d {
				n++
			}
		}
		return n
	}
	changed := (prior != "" && count(prior) == 1) || (did != "" && count(did) == 0)
	if did == "" {
		delete(vms, name)
	} else {
		vms[name] = did
	}
	return changed
}

func ComputeVMWatch(c *Composer, export, rootKind string) ctlruntime.Option {
	return ctlruntime.WatchesIn(export, func(_ context.Context, src ctlruntime.Request, vm *v1alpha1.ComputeVM) []ctlruntime.Request {
		out := c.deletingIn(rootKind, src.Workspace)
		did := ""
		if liveComputeVM(vm) {
			did = strings.TrimSpace(vm.Spec.Requester.Did)
		}
		if c.requesters.note(src.Cluster+"|"+src.Workspace, vm.Name, did) {
			for _, name := range c.rootsIn(src.Cluster, rootKind) {
				out = append(out, ctlruntime.Request{Cluster: src.Cluster, Name: name})
			}
		}
		return out
	})
}

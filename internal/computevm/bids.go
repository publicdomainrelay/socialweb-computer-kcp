package computevm

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	BidTTL         = "24h"
	BidFinishEvery = time.Minute
	BidLinkEvery   = 10 * time.Second
	WonUnboundWait = time.Hour
)

var BidGVR = v1alpha1.BidGVR

func bidFinish(o *unstructured.Unstructured, vms map[string]bool, now time.Time) (string, time.Time, string, bool) {
	phase, _, _ := unstructured.NestedString(o.Object, "status", "phase")
	finished := func() time.Time {
		stamp, _, _ := unstructured.NestedString(o.Object, "status", "timings", "finishedAt")
		if at, err := time.Parse(time.RFC3339, stamp); err == nil {
			return at
		}
		return o.GetCreationTimestamp().Time
	}
	switch v1alpha1.BidPhase(phase) {
	case v1alpha1.BidPhaseLost, v1alpha1.BidPhaseExpired:
		return v1alpha1.BidConditionComplete, finished(), phase, true
	case v1alpha1.BidPhaseFailed:
		return v1alpha1.BidConditionFailed, finished(), phase, true
	case v1alpha1.BidPhaseWon:
		vm, _, _ := unstructured.NestedString(o.Object, "status", "computeVM")
		if vm != "" {
			if vms[vm] {
				return "", time.Time{}, "", false
			}
			return v1alpha1.BidConditionComplete, now, "ComputeVMGone", true
		}
		stamp, _, _ := unstructured.NestedString(o.Object, "status", "timings", "acceptedAt")
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil || now.Sub(at) < WonUnboundWait {
			return "", time.Time{}, "", false
		}
		return v1alpha1.BidConditionComplete, now, "NeverBound", true
	}
	return "", time.Time{}, "", false
}

func hasFinishedCondition(o *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); (t == v1alpha1.BidConditionComplete || t == v1alpha1.BidConditionFailed) && m["status"] == "True" {
			return true
		}
	}
	return false
}

func hasOwnerKind(o *unstructured.Unstructured, kind string) bool {
	for _, r := range o.GetOwnerReferences() {
		if r.Kind == kind {
			return true
		}
	}
	return false
}

func (ct *Controller) linkBids(ctx context.Context, vms []unstructured.Unstructured) {
	loose := map[string]*unstructured.Unstructured{}
	for i := range vms {
		vm := &vms[i]
		if vm.GetDeletionTimestamp() != nil || hasOwnerKind(vm, "Bid") {
			continue
		}
		if h, _, _ := unstructured.NestedString(vm.Object, "spec", "owner", "handle"); h != ct.c.Handle {
			continue
		}
		loose[vm.GetName()] = vm
	}
	ct.mu.Lock()
	due := len(loose) > 0 && time.Since(ct.linksAt) >= BidLinkEvery
	if due {
		ct.linksAt = time.Now()
	}
	ct.mu.Unlock()
	if !due {
		return
	}
	log := ct.c.log()
	list, err := ct.bids.List(ctx, metav1.ListOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			log.Warn("listing Bids to hang ComputeVMs from them", "err", err.Error())
		}
		return
	}
	byAccept := map[string]string{}
	for name, vm := range loose {
		if accept, _, _ := strings.Cut(vm.GetAnnotations()[AcceptAnnotation], "#"); accept != "" {
			byAccept[accept] = name
		}
	}
	for i := range list.Items {
		bid := &list.Items[i]
		name, _, _ := unstructured.NestedString(bid.Object, "status", "computeVM")
		unbound := name == ""
		if unbound {
			accept, _, _ := unstructured.NestedString(bid.Object, "status", "accept", "uri")
			name = byAccept[accept]
		}
		vm := loose[name]
		if vm == nil || bid.GetDeletionTimestamp() != nil {
			continue
		}
		if unbound {
			next := bid.DeepCopy()
			if err := unstructured.SetNestedField(next.Object, name, "status", "computeVM"); err != nil {
				continue
			}
			if _, err := ct.bids.UpdateStatus(ctx, next, metav1.UpdateOptions{}); err != nil {
				if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
					log.Warn("binding a Won Bid to the ComputeVM that carries its accept", "bid", bid.GetName(), "computevm", name, "err", err.Error())
				}
				continue
			}
		}
		refs := append(vm.GetOwnerReferences(), metav1.OwnerReference{APIVersion: bid.GetAPIVersion(), Kind: "Bid", Name: bid.GetName(), UID: bid.GetUID()})
		body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(vm.GetUID()), "ownerReferences": refs}})
		if err != nil {
			continue
		}
		if _, err := ct.vms.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
			if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				log.Warn("hanging a ComputeVM from its won Bid", "computevm", name, "bid", bid.GetName(), "err", err.Error())
			}
			continue
		}
		log.Info("a ComputeVM hangs from its won Bid", "computevm", name, "bid", bid.GetName())
	}
}

func (ct *Controller) finishBids(ctx context.Context, vms []unstructured.Unstructured) {
	ct.mu.Lock()
	due := time.Since(ct.bidsAt) >= BidFinishEvery
	if due {
		ct.bidsAt = time.Now()
	}
	ct.mu.Unlock()
	if !due {
		return
	}
	log := ct.c.log()
	list, err := ct.bids.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		log.Warn("listing Bids to mark the finished ones", "err", err.Error())
		return
	}
	live := map[string]bool{}
	for i := range vms {
		live[vms[i].GetName()] = true
	}
	now := time.Now().UTC()
	var marked []string
	for i := range list.Items {
		o := &list.Items[i]
		if o.GetDeletionTimestamp() != nil {
			continue
		}
		if _, ok := o.GetAnnotations()[v1alpha1.AnnotationTTLAfterFinished]; !ok {
			body, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": string(o.GetUID()),
				"annotations": map[string]string{v1alpha1.AnnotationTTLAfterFinished: BidTTL}}})
			if err == nil {
				ct.bids.Patch(ctx, o.GetName(), types.MergePatchType, body, metav1.PatchOptions{})
			}
		}
		if hasFinishedCondition(o) {
			continue
		}
		kind, at, reason, done := bidFinish(o, live, now)
		if !done {
			continue
		}
		conds, _, _ := unstructured.NestedSlice(o.Object, "status", "conditions")
		conds = append(conds, map[string]any{"type": kind, "status": "True", "reason": reason, "message": "the Bid is finished; its TTL runs from now",
			"lastTransitionTime": at.UTC().Format(time.RFC3339), "observedGeneration": o.GetGeneration()})
		next := o.DeepCopy()
		if err := unstructured.SetNestedSlice(next.Object, conds, "status", "conditions"); err != nil {
			continue
		}
		if _, err := ct.bids.UpdateStatus(ctx, next, metav1.UpdateOptions{}); err != nil {
			if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				log.Warn("marking a finished Bid", "bid", o.GetName(), "err", err.Error())
			}
			continue
		}
		marked = append(marked, o.GetName())
	}
	if len(marked) > 0 {
		log.Info("finished Bids carry their condition; the TTL controller removes them after their TTL", "bids", len(marked), "ttl", BidTTL)
	}
}

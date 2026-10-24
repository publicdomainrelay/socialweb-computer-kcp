package request

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/webrtcproxy"
)

const contractEventPrefix = "cli-"

var _ webrtcproxy.Signaling = (*ContractSignaling)(nil)

type ContractSignaling struct {
	k *Kcp

	contract string

	session string
}

func (k *Kcp) Signaling() *ContractSignaling {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &ContractSignaling{k: k, contract: k.ContractName(), session: hex.EncodeToString(b[:])}
}

func (s *ContractSignaling) create(ctx context.Context, suffix string, spec v1alpha1.ContractEventSpec) error {
	spec.ContractRef = commonv1.LocalObjectRef{Name: s.contract}
	spec.Direction = v1alpha1.ContractEventOutbound
	event := v1alpha1.ContractEvent{TypeMeta: typeMeta("ContractEvent"),
		ObjectMeta: metav1.ObjectMeta{Name: contractEventPrefix + s.session + "-" + suffix}, Spec: spec}
	body, err := kube.ApplyBody(&event, false)
	if err != nil {
		return err
	}
	_, err = kube.Create[kube.Doc](ctx, s.k.dyn.Resource(v1alpha1.ContractEventGVR), body, metav1.CreateOptions{FieldManager: FieldManager})
	return err
}

func (s *ContractSignaling) PublishKey(ctx context.Context, tag, key string) error {
	return s.create(ctx, "key", v1alpha1.ContractEventSpec{NSID: v1alpha1.ContractEventNSIDGuestPublicKey,
		Kind: market.GuestPublicKeyKindAgeX25519, Tag: tag, Key: key})
}

func (s *ContractSignaling) PublishOffer(ctx context.Context, tag, ciphertext string) error {
	return s.create(ctx, "offer", v1alpha1.ContractEventSpec{NSID: v1alpha1.ContractEventNSIDGuestSignalWebRTC,
		Kind: market.GuestSignalKindOffer, Tag: tag, Ciphertext: ciphertext})
}

func (s *ContractSignaling) inbound(e v1alpha1.ContractEvent, nsid, kind, tag string) bool {
	return e.DeletionTimestamp == nil && e.Spec.Direction == v1alpha1.ContractEventInbound && e.Spec.ContractRef.Name == s.contract &&
		e.Spec.NSID == nsid && e.Spec.Kind == kind && e.Spec.Tag == tag && e.Status.Phase == v1alpha1.ContractEventReceived
}

func (s *ContractSignaling) Guest(ctx context.Context, tag string) (webrtcproxy.Guest, error) {
	var guest webrtcproxy.Guest
	var failed error
	_, err := kube.Await(ctx, s.k.dyn.Resource(v1alpha1.ComputeContractGVR), s.contract, 0, func(c *v1alpha1.ComputeContract) (bool, error) {
		if c == nil {
			return false, nil
		}
		switch c.Status.Phase {
		case v1alpha1.ComputeContractReleased, v1alpha1.ComputeContractFailed:
			failed = fmt.Errorf("the contract is %s: %s", c.Status.Phase, c.Status.Message)
			return true, nil
		}
		if c.Status.Contract == nil {
			return false, nil
		}
		for _, n := range c.Status.Contract.Networks {
			if n.Tag == tag && n.Kind == v1alpha1.NetworkKindWebRTC && strings.TrimSpace(n.Address) != "" {
				guest.Fingerprint = n.Address
				return true, nil
			}
		}
		return false, nil
	})
	if failed != nil {
		return guest, failed
	}
	if err != nil {
		return guest, fmt.Errorf("no onNetwork kind webrtc for the tag in status.contract.networks: %w", err)
	}
	_, err = kube.AwaitList(ctx, s.k.dyn.Resource(v1alpha1.ContractEventGVR), metav1.ListOptions{}, 0, func(items []v1alpha1.ContractEvent) (bool, error) {
		var newest *v1alpha1.ContractEvent
		for i := range items {
			e := &items[i]
			if !s.inbound(*e, v1alpha1.ContractEventNSIDGuestPublicKey, market.GuestPublicKeyKindAgeX25519, tag) || e.Spec.Key == "" {
				continue
			}
			if newest == nil || newest.CreationTimestamp.Before(&e.CreationTimestamp) {
				newest = e
			}
		}
		if newest == nil {
			return false, nil
		}
		guest.Key = newest.Spec.Key
		return true, nil
	})
	if err != nil {
		return guest, fmt.Errorf("no inbound guest publicKey ContractEvent for the tag: %w", err)
	}
	return guest, nil
}

func (s *ContractSignaling) Answers(ctx context.Context, tag string, each func(ciphertext string) bool) error {
	seen := map[string]bool{}
	_, err := kube.AwaitList(ctx, s.k.dyn.Resource(v1alpha1.ContractEventGVR), metav1.ListOptions{}, 0, func(items []v1alpha1.ContractEvent) (bool, error) {
		for _, e := range items {
			key := e.Name + "/" + string(e.UID)
			if seen[key] || !s.inbound(e, v1alpha1.ContractEventNSIDGuestSignalWebRTC, market.GuestSignalKindAnswer, tag) || e.Spec.Ciphertext == "" {
				continue
			}
			seen[key] = true
			if each(e.Spec.Ciphertext) {
				return true, nil
			}
		}
		return false, nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return context.DeadlineExceeded
	}
	return err
}

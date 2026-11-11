package requester

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/files"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds/plcsubmit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/identlink"
)

const (
	RenterTombstoneWait = 2 * time.Minute

	renterTombstonePoll = 10 * time.Second

	maxGuestAccounts = 16
)

func (q *Requester) renterRotationKey() (atcrypto.PrivateKey, error) {
	dir := q.cfg.dataDir()
	if dir == "" {
		return nil, errors.New("the requester has no state dir for its PLC rotation key")
	}
	return files.RotationKey(dir, "")
}

func (q *Requester) renterRotationDID() string {
	key, err := q.renterRotationKey()
	if err == nil {
		var pub atcrypto.PublicKey
		if pub, err = key.PublicKey(); err == nil {
			return pub.DIDKey()
		}
	}
	q.log.Warn("requester_rotation_key_failed", "err", err.Error(), "next", "the guest's DIDs get no renter rotation key")
	return ""
}

func (q *Requester) plcDirectoryURL() string {
	if network := q.cfg.Spec.Network; network != nil && strings.TrimSpace(network.PLCDirectoryURL) != "" {
		return strings.TrimRight(strings.TrimSpace(network.PLCDirectoryURL), "/")
	}
	return PublicPLCDirectoryURL
}

func GuestDIDs(contract *v1alpha1.ComputeContract) []string {
	if contract == nil || contract.Status.Contract == nil {
		return nil
	}
	g := contract.Status.Contract.GuestRequester
	if g == nil || g.State != v1alpha1.ContractGuestRequesterVerified {
		return nil
	}
	return append([]string{g.Did}, guestAccounts(g.Accounts, "", g.Did)...)
}

func guestAccounts(accounts []string, renter, r2 string) []string {
	out := []string{}
	for _, did := range accounts {
		if strings.HasPrefix(did, "did:plc:") && did != renter && did != r2 && !slices.Contains(out, did) && len(out) < maxGuestAccounts {
			out = append(out, did)
		}
	}
	return out
}

func (r *contractRunner) tombstoneFallback(state *v1alpha1.NodeClaimContract) {
	q := r.q
	if !fromGuest(r.snapshot()) || state == nil || state.GuestRequester == nil || state.GuestRequester.State != v1alpha1.ContractGuestRequesterVerified {
		return
	}
	r2 := state.GuestRequester.Did
	principals := map[string]string{r2: identlink.AgentOfPrefix + q.repoDID}
	for _, did := range guestAccounts(state.GuestRequester.Accounts, q.repoDID, r2) {
		principals[did] = identlink.AgentOfPrefix + r2
	}
	wait := RenterTombstoneWait
	if state.Termination != nil {
		wait = 0
	}
	r.tombstoneOnce.Do(func() {
		q.wg.Go(func() { r.tombstoneGuest(principals, wait) })
	})
}

func (r *contractRunner) tombstoneGuest(principals map[string]string, wait time.Duration) {
	q := r.q
	ctx, cancel := context.WithTimeout(context.WithoutCancel(q.runContext()), wait+time.Minute)
	defer cancel()
	key, err := q.renterRotationKey()
	if err != nil {
		r.log().Warn("requester_tombstone_skipped", "reason", err.Error())
		return
	}
	submitter, err := plcsubmit.New(q.plcDirectoryURL(), key, q.marketHTTP())
	if err != nil {
		r.log().Warn("requester_tombstone_skipped", "reason", err.Error())
		return
	}
	deadline := time.Now().Add(wait)
	for {
		for did := range principals {
			if head, err := submitter.Head(ctx, syntax.DID(did)); err == nil && head.Type == plc.TypeTombstone {
				delete(principals, did)
			}
		}
		if len(principals) == 0 {
			r.log().Info("requester_guest_dids_tombstoned", "by", "the guest")
			return
		}
		if st := r.stateCopy(); !time.Now().Before(deadline) || (st != nil && st.Termination != nil) || sleepCtx(ctx, renterTombstonePoll) != nil {
			break
		}
	}
	for did, agentOf := range principals {
		head, err := submitter.Head(ctx, syntax.DID(did))
		switch {
		case err != nil:
			r.log().Warn("requester_tombstone_failed", "did", did, "step", "read the PLC log", "err", err.Error())
			continue
		case head.Type != plc.TypeOperation || !slices.Contains(head.AlsoKnownAs, agentOf):
			r.log().Warn("requester_tombstone_skipped", "did", did, "reason", "its DID document does not say "+agentOf)
			continue
		case !slices.Contains(head.RotationKeys, submitter.RotationDID()):
			r.log().Warn("requester_tombstone_skipped", "did", did, "reason", "the renter rotation key "+submitter.RotationDID()+" is not one of its rotation keys")
			continue
		}
		if err := submitter.Tombstone(ctx, syntax.DID(did)); err != nil {
			r.log().Warn("requester_tombstone_failed", "did", did, "err", err.Error())
			continue
		}
		r.log().Info("requester_tombstoned", "did", did, "by", "the renter rotation key", "key", submitter.RotationDID())
	}
}

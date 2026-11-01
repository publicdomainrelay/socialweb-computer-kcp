package requester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const (
	atircIndexKey = "bundles"

	orphanSweepAttempts = 10
)

type atircIndexEntry struct {
	Contract string `json:"contract"`

	UID string `json:"uid,omitempty"`

	SecretsRef commonv1.SecretRef `json:"secretsRef"`

	Exp int64 `json:"exp,omitempty"`
}

func (q *Requester) atircMintedRef() commonv1.SecretRef {
	if ref := q.cfg.Spec.AtircMintedRef; ref != nil {
		return *ref
	}
	out := commonv1.SecretRef{Mount: DefaultSecretsMount, Path: AtircMintedPath}
	if parent := q.cfg.Spec.AtircParentRef; parent != nil && parent.Mount != "" {
		out.Mount, out.Vault = parent.Mount, parent.Vault.DeepCopy()
	}
	return out
}

func vaultPathGone(err error) bool {
	return err != nil && errors.Is(err, vault.ErrNotFound) && !errors.Is(err, vault.ErrMountNotReady)
}

var errVaultNoData = errors.New("the vault answered no data and no error")

func (q *Requester) readAtircIndex(ctx context.Context) (vault.KV, commonv1.SecretRef, []atircIndexEntry, error) {
	ref := q.atircMintedRef()
	kv, err := q.deps.VaultWrite(ctx, ref)
	if err != nil {
		return nil, ref, nil, err
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	switch {
	case vaultPathGone(err):
		return kv, ref, nil, nil
	case err != nil:
		return nil, ref, nil, err
	case data == nil:
		return nil, ref, nil, errVaultNoData
	}
	text, _ := data[atircIndexKey].(string)
	if text == "" {
		return kv, ref, nil, nil
	}
	var entries []atircIndexEntry
	if err := json.Unmarshal([]byte(text), &entries); err != nil {
		return nil, ref, nil, fmt.Errorf("parse the atirc minted index: %w", err)
	}
	return kv, ref, entries, nil
}

func writeAtircIndex(ctx context.Context, kv vault.KV, ref commonv1.SecretRef, entries []atircIndexEntry) error {
	if entries == nil {
		entries = []atircIndexEntry{}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return kv.Write(ctx, ref.Mount, ref.Path, map[string]any{atircIndexKey: string(raw)})
}

func (e atircIndexEntry) same(o atircIndexEntry) bool {
	return e.Contract == o.Contract && e.UID == o.UID && e.SecretsRef == o.SecretsRef &&
		(e.SecretsRef.Vault == nil) == (o.SecretsRef.Vault == nil) && (e.SecretsRef.Vault == nil || *e.SecretsRef.Vault == *o.SecretsRef.Vault)
}

func (q *Requester) indexMinted(ctx context.Context, contract v1alpha1.ComputeContract, exp time.Time) error {
	if contract.Spec.SecretsRef == nil {
		return errors.New("the contract has no secrets bundle to index")
	}
	add := atircIndexEntry{Contract: contract.Name, UID: string(contract.UID), SecretsRef: *contract.Spec.SecretsRef.DeepCopy(), Exp: exp.Unix()}
	q.atircIndexMu.Lock()
	defer q.atircIndexMu.Unlock()
	kv, ref, entries, err := q.readAtircIndex(ctx)
	if err != nil {
		return err
	}
	for i, e := range entries {
		if e.same(add) {
			if e.Exp >= add.Exp {
				return nil
			}
			entries[i].Exp = add.Exp
			return writeAtircIndex(ctx, kv, ref, entries)
		}
	}
	return writeAtircIndex(ctx, kv, ref, append(entries, add))
}

func (q *Requester) dropIndexed(ctx context.Context, done []atircIndexEntry) error {
	if len(done) == 0 {
		return nil
	}
	q.atircIndexMu.Lock()
	defer q.atircIndexMu.Unlock()
	kv, ref, entries, err := q.readAtircIndex(ctx)
	if err != nil {
		return err
	}
	kept := make([]atircIndexEntry, 0, len(entries))
	for _, e := range entries {
		gone := false
		for _, d := range done {
			if e.same(d) {
				gone = true
			}
		}
		if !gone {
			kept = append(kept, e)
		}
	}
	if len(kept) == len(entries) {
		return nil
	}
	return writeAtircIndex(ctx, kv, ref, kept)
}

func (q *Requester) startOrphanRevokes() {
	if q.deps.VaultWrite == nil {
		return
	}
	q.orphanSweep.Do(func() {
		q.wg.Go(q.sweepOrphanRevokes)
	})
}

func (q *Requester) sweepOrphanRevokes() {
	stop := q.runContext()
	ref := q.atircMintedRef()
	q.log.Info("requester_atirc_orphan_sweep", "index", ref.Mount+"/"+ref.Path)
	for attempt := 1; ; attempt++ {
		left, err := q.sweepOrphansOnce(stop)
		if err == nil && left == 0 {
			return
		}
		q.log.Warn("requester_atirc_orphan_sweep_retry", "attempt", attempt, "left", left, "err", fmt.Sprint(err))
		if attempt >= orphanSweepAttempts || !sleepRetry(stop, attempt) {
			q.log.Warn("requester_atirc_orphan_sweep_left", "left", left, "reason", "the next requester start sweeps again")
			return
		}
	}
}

func sleepRetry(stop context.Context, attempt int) bool {
	return sleepCtx(stop, min(associationRetryFirst<<min(attempt-1, 10), associationRetryMax)) == nil
}

func (q *Requester) sweepOrphansOnce(stop context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(stop, atircCallTimeout)
	q.atircIndexMu.Lock()
	_, _, entries, err := q.readAtircIndex(ctx)
	q.atircIndexMu.Unlock()
	cancel()
	if err != nil {
		return 1, fmt.Errorf("read the atirc minted index: %w", err)
	}
	var done []atircIndexEntry
	left := 0
	for _, entry := range entries {
		if q.orphanOwnedByRunner(entry) {
			continue
		}
		ctx, cancel := context.WithTimeout(stop, 4*atircCallTimeout)
		cleared, err := q.revokeOrphan(ctx, entry)
		cancel()
		if (err != nil || !cleared) && entry.Exp > 0 && !q.now().Before(time.Unix(entry.Exp, 0)) {
			q.log.Info("requester_atirc_orphan_expired", "contract", entry.Contract, "uid", entry.UID, "err", fmt.Sprint(err))
			done = append(done, entry)
			continue
		}
		if err != nil || !cleared {
			left++
			q.log.Warn("requester_atirc_orphan_revoke_pending", "contract", entry.Contract, "uid", entry.UID, "err", fmt.Sprint(err))
			continue
		}
		done = append(done, entry)
	}
	ctx, cancel = context.WithTimeout(stop, atircCallTimeout)
	defer cancel()
	if err := q.dropIndexed(ctx, done); err != nil {
		return left + len(done), fmt.Errorf("drop the cleared bundles from the atirc minted index: %w", err)
	}
	return left, nil
}

func (q *Requester) orphanOwnedByRunner(entry atircIndexEntry) bool {
	live, ok := q.contracts.Get(entry.Contract)
	if !ok || (entry.UID != "" && string(live.UID) != entry.UID) {
		return false
	}
	if !contractTerminal(live) {
		return true
	}
	state := live.Status.AtircRevokes
	return state != v1alpha1.AtircRevokesDone && state != v1alpha1.AtircRevokesAbandoned
}

func (q *Requester) revokeOrphan(ctx context.Context, entry atircIndexEntry) (bool, error) {
	live, liveOK := q.contracts.Get(entry.Contract)
	otherLive := liveOK && entry.UID != "" && string(live.UID) != entry.UID
	mine := func(m atircMinted) bool {
		return m.UID == entry.UID || (m.UID == "" && !otherLive)
	}
	contract := v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: entry.Contract, UID: types.UID(entry.UID)},
		Spec: v1alpha1.ComputeContractSpec{SecretsRef: entry.SecretsRef.DeepCopy()}}
	r := newContractRunner(q, contract)
	kv, err := q.deps.VaultWrite(ctx, entry.SecretsRef)
	if err != nil {
		return false, fmt.Errorf("the vault client of the bundle: %w", err)
	}
	r.bundleMu.Lock()
	data, err := kv.Read(ctx, entry.SecretsRef.Mount, entry.SecretsRef.Path, 0)
	r.bundleMu.Unlock()
	switch {
	case vaultPathGone(err):
		q.log.Info("requester_atirc_orphan_cleared", "contract", entry.Contract, "uid", entry.UID, "reason", "the bundle is gone")
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read the bundle: %w", err)
	case data == nil:
		return false, fmt.Errorf("read the bundle: %w", errVaultNoData)
	}
	minted, err := mintedOf(data)
	if err != nil {
		return false, err
	}
	revoked := 0
	parent := r.revokeParent(ctx, contract)
	for _, m := range minted {
		if !mine(m) || !q.now().Before(time.Unix(m.Exp, 0)) {
			continue
		}
		if _, err := r.revokeMinted(ctx, parent, m.Token, m.Subhandle); err != nil {
			return false, fmt.Errorf("revoke %s: %w", m.Subhandle, err)
		}
		revoked++
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	data, err = kv.Read(ctx, entry.SecretsRef.Mount, entry.SecretsRef.Path, 0)
	switch {
	case vaultPathGone(err):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read the bundle again: %w", err)
	case data == nil:
		return false, fmt.Errorf("read the bundle again: %w", errVaultNoData)
	}
	current, err := mintedOf(data)
	if err != nil {
		return false, err
	}
	var kept []atircMinted
	for _, m := range current {
		if !mine(m) {
			kept = append(kept, m)
		}
	}
	next := make(map[string]any, len(data))
	for k, v := range data {
		next[k] = v
	}
	delete(next, atircMintedKey)
	if len(kept) > 0 {
		raw, err := json.Marshal(kept)
		if err != nil {
			return false, err
		}
		next[atircMintedKey] = string(raw)
	}
	if marker, err := markerIn(data); err == nil && marker != nil &&
		((entry.UID != "" && marker.UID == entry.UID) || (marker.UID == "" && !otherLive)) {
		delete(next, atircReleaseKey)
	}
	if len(next) != len(data) || len(kept) != len(current) {
		if err := kv.Write(ctx, entry.SecretsRef.Mount, entry.SecretsRef.Path, next); err != nil {
			return false, fmt.Errorf("clear the minted tokens: %w", err)
		}
	}
	q.log.Info("requester_atirc_orphan_cleared", "contract", entry.Contract, "uid", entry.UID, "revoked", revoked, "kept", len(kept))
	return true, nil
}

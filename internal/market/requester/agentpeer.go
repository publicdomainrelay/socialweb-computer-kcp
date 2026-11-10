package requester

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
)

const (
	AgentPeerNSID = "computer.socialweb.peer"

	AtircNickEnv = "ATIRC_NICK"

	AtircUsernameEnv = "ATP_USERNAME"

	AtircAgentOperatorNSID = "com.fedfork.irc.agentOperator"

	AtircAgentsNSID = "com.fedfork.irc.agents"

	agentPeerPublished = "PeerPublished"

	maxOwnerHops = 16
)

func AtircDefaultChannel(handle string) string {
	var b strings.Builder
	b.WriteByte('#')
	for i, c := range []rune(handle) {
		if i == 63 {
			break
		}
		switch c {
		case '.', ',', 0, ' ', '\t', '\n', '\r':
			b.WriteByte('_')
		default:
			b.WriteRune(c)
		}
	}
	if b.Len() == 1 {
		return "#atirc"
	}
	return strings.ToLower(b.String())
}

func (r *contractRunner) agentChannel(ctx context.Context) string {
	q := r.q
	did, handle := q.repoDID, strings.ToLower(strings.TrimSpace(q.handle))
	for range maxOwnerHops {
		op, err := q.repo.GetRecord(ctx, did, AtircAgentOperatorNSID, "self")
		if err != nil {
			break
		}
		owner, _ := op.Value["owner"].(string)
		if !strings.HasPrefix(owner, "did:") || owner == did || owner == q.repoDID {
			break
		}
		if _, err := q.repo.GetRecord(ctx, owner, AtircAgentsNSID, did); err != nil {
			break
		}
		did = owner
	}
	if did != q.repoDID && q.deps.Directory != nil {
		if parsed, err := syntax.ParseDID(did); err == nil {
			if ident, err := q.deps.Directory.LookupDID(ctx, parsed); err == nil && ident.Handle.String() != syntax.HandleInvalid.String() {
				handle = strings.ToLower(ident.Handle.String())
			}
		}
	}
	return AtircDefaultChannel(handle)
}

func agentNick(handle, rfpURI string) string {
	label, _, _ := strings.Cut(handle, ".")
	if len(label) > maxAtircLabel {
		label = label[:maxAtircLabel]
	}
	label, rkey := strings.TrimRight(label, "-"), strings.ToLower(uriRKey(rfpURI))
	if label == "" || rkey == "" {
		return ""
	}
	return label + "-" + rkey
}

func (r *contractRunner) agentIdentity(state *v1alpha1.NodeClaimContract) (string, string) {
	handle := strings.ToLower(strings.TrimSpace(r.q.handle))
	if state == nil || state.RFPURI == "" || handle == "" {
		return "", ""
	}
	nick := agentNick(handle, state.RFPURI)
	if nick == "" {
		return "", ""
	}
	return nick, nick + "." + handle
}

func agentPeerReady(entries []SecretEntry, envFile string) bool {
	env, key := false, false
	for _, e := range entries {
		switch e.Path {
		case envFile:
			env = atircEnvValue(e.Value, AtircNickEnv) != ""
		case cloudinit.AgentIrohKeyFile:
			key = strings.TrimSpace(e.Value) != ""
		}
	}
	return env && key
}

func (r *contractRunner) ensureAgentPeer(ctx context.Context, contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract) {
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" || r.released() {
		return
	}
	if err := r.publishAgentPeer(ctx, contract, state, envFile); err == nil {
		return
	} else if !r.notePeerFailure(ctx, 1, err) {
		return
	}
	if _, retrying := r.q.atircMintRetrying.LoadOrStore(contract.Name, true); retrying {
		return
	}
	r.q.wg.Go(func() {
		defer r.q.atircMintRetrying.Delete(contract.Name)
		for attempt := 2; attempt <= associationRetryAttempts; attempt++ {
			if !sleepRetry(ctx, attempt-1) || r.released() {
				return
			}
			err := r.publishAgentPeer(ctx, r.snapshot(), state, envFile)
			if err == nil {
				r.log().Info("requester_agent_peer_published_after_retry", "attempt", attempt)
				return
			}
			if !r.notePeerFailure(ctx, attempt, err) {
				return
			}
		}
		r.log().Warn("requester_agent_peer_gave_up", "attempts", associationRetryAttempts)
		r.noteAgentMint(ctx, false, "GaveUp", fmt.Sprintf("no atirc agent peer record after %d attempts", associationRetryAttempts))
	})
}

var errAgentPeerNotYet = errors.New("the contract has no verified receipt; the agent's peer record waits for it")

func (r *contractRunner) notePeerFailure(ctx context.Context, attempt int, err error) bool {
	r.log().Warn("requester_agent_peer_failed", "attempt", attempt, "err", err.Error())
	r.noteAgentMint(ctx, false, "PeerFailed", fmt.Sprintf("the atirc agent peer record is not published (attempt %d): %v", attempt, err))
	return !errors.Is(err, errAgentPeerNotYet)
}

func (r *contractRunner) publishAgentPeer(ctx context.Context, contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract, envFile string) error {
	q := r.q
	if _, err := contractLease(state, nil); err != nil || state.Accept == nil || state.Accept.URI == "" {
		return errAgentPeerNotYet
	}
	switch {
	case q.repo == nil:
		return errors.New("the requester has no repo handle")
	case q.deps.VaultWrite == nil:
		return errors.New("the requester has no vault write handle")
	case contract.Spec.SecretsRef == nil:
		return errAgentWithoutSecrets
	}
	nick, identity := r.agentIdentity(state)
	if identity == "" {
		return fmt.Errorf("the handle %q and the RFP %q make no agent identity", q.handle, state.RFPURI)
	}
	seed, fresh, err := r.agentKey(ctx, contract, envFile, nick)
	if err != nil {
		return err
	}
	lease, err := r.agentLease(ctx, contract, *state.Accept)
	if err != nil {
		return fmt.Errorf("the agent's lease: %w", err)
	}
	ticket := clusterlink.StableTicket(clusterlink.Key{Private: ed25519.NewKeyFromSeed(seed)})
	channel := r.agentChannel(ctx)
	if existing, err := q.repo.GetRecord(ctx, q.repoDID, AgentPeerNSID, identity); err == nil {
		got, _ := existing.Value["lease"].(map[string]any)
		channels, _ := existing.Value["channels"].([]any)
		if existing.Value["iroh"] == ticket && len(channels) == 1 && channels[0] == channel && got != nil && got["uri"] == lease.URI && got["cid"] == lease.CID {
			r.agentPeerDone(ctx, contract, envFile, fresh, identity, "present")
			return nil
		}
	} else if !errors.Is(err, market.ErrNotFound) {
		return fmt.Errorf("read %s/%s: %w", AgentPeerNSID, identity, err)
	}
	record := map[string]any{"$type": AgentPeerNSID, "iroh": ticket, "channels": []any{channel},
		"lease": map[string]any{"uri": lease.URI, "cid": lease.CID}, "createdAt": market.FormatTime(q.now())}
	written, err := q.repo.PutRecord(ctx, q.repoDID, AgentPeerNSID, identity, record)
	if err != nil {
		return fmt.Errorf("write %s/%s: %w", AgentPeerNSID, identity, err)
	}
	r.log().Info("requester_agent_peer_published", "identity", identity, "uri", written.URI, "lease", lease.URI, "channel", channel)
	r.agentPeerDone(ctx, contract, envFile, fresh, identity, "written")
	return nil
}

func (r *contractRunner) agentPeerDone(ctx context.Context, contract v1alpha1.ComputeContract, envFile string, fresh bool, identity, how string) {
	notify := fresh
	r.mu.Lock()
	secrets := r.secrets
	r.mu.Unlock()
	for _, p := range []string{envFile, cloudinit.AgentIrohKeyFile} {
		if secrets.minted(p) {
			notify = true
		}
	}
	if notify {
		if err := r.notifySecretsUpdated(ctx, contract, commonv1.StrongRef{URI: envFile}); err != nil {
			r.log().Warn("requester_agent_peer_notify_failed", "err", err.Error())
		}
	}
	r.noteAgentMint(ctx, true, agentPeerPublished, "the atirc agent's mesh key is in the bundle and "+AgentPeerNSID+"/"+identity+" is "+how)
}

func (r *contractRunner) agentKey(ctx context.Context, contract v1alpha1.ComputeContract, envFile, nick string) ([]byte, bool, error) {
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return nil, false, fmt.Errorf("read the bundle: %w", err)
	}
	var seed []byte
	envAt, keyAt := -1, -1
	for i, e := range entries {
		switch e.Path {
		case envFile:
			envAt = i
		case cloudinit.AgentIrohKeyFile:
			keyAt = i
			if b, err := hex.DecodeString(strings.TrimSpace(e.Value)); err == nil && len(b) == ed25519.SeedSize {
				seed = b
			}
		}
	}
	env := AtircUsernameEnv + "=" + strings.ToLower(strings.TrimSpace(r.q.handle)) + "\n" + AtircNickEnv + "=" + nick + "\n"
	if seed != nil && envAt >= 0 && entries[envAt].Value == env {
		r.addSecretEntries(entries[envAt], entries[keyAt])
		return seed, false, nil
	}
	if seed == nil {
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, false, err
		}
	}
	envEntry := SecretEntry{Path: envFile, Value: env, Mode: "0600"}
	keyEntry := SecretEntry{Path: cloudinit.AgentIrohKeyFile, Value: hex.EncodeToString(seed) + "\n", Mode: "0600"}
	if envAt < 0 {
		entries = append(entries, envEntry)
	} else {
		entries[envAt] = envEntry
	}
	if keyAt < 0 {
		entries = append(entries, keyEntry)
	} else {
		entries[keyAt] = keyEntry
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, false, err
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return nil, false, fmt.Errorf("write the bundle: %w", err)
	}
	r.addSecretEntries(envEntry, keyEntry)
	r.log().Info("requester_agent_mesh_key_written", "envFile", envFile, "keyFile", cloudinit.AgentIrohKeyFile, "mode", "0600")
	return seed, true, nil
}

func (r *contractRunner) agentLease(ctx context.Context, contract v1alpha1.ComputeContract, accept commonv1.StrongRef) (commonv1.StrongRef, error) {
	q := r.q
	rkey := ContractAssociationRKey(q.repoDID, accept)
	r.mu.Lock()
	if r.associationRecords == nil {
		r.associationRecords = map[string]string{}
	}
	r.associationRecords[rkey] = q.repoDID
	r.mu.Unlock()
	existing, err := q.repo.GetRecord(ctx, q.repoDID, market.AssociationNSID, rkey)
	switch {
	case err == nil:
		bound, ok := recordContract(existing.Value)
		if !ok || bound != accept {
			return commonv1.StrongRef{}, fmt.Errorf("%s is bound to another contract", existing.URI)
		}
		exp, _ := existing.Value["expiresAt"].(string)
		if t, err := market.ParseTime(exp); err == nil && q.now().Add(time.Hour).Before(t) {
			return commonv1.StrongRef{URI: existing.URI, CID: existing.CID}, nil
		}
	case !errors.Is(err, market.ErrNotFound):
		return commonv1.StrongRef{}, err
	}
	written, err := q.repo.PutRecord(ctx, q.repoDID, market.AssociationNSID, rkey, map[string]any{
		"$type":     market.AssociationNSID,
		"subject":   q.repoDID,
		"service":   trust.RequesterAssociate,
		"contract":  map[string]any{"uri": accept.URI, "cid": accept.CID},
		"expiresAt": market.FormatTime(q.now().Add(ContractAssociationLifetime)),
		"createdAt": market.FormatTime(q.now()),
	})
	if err != nil {
		return commonv1.StrongRef{}, err
	}
	r.log().Info("requester_agent_lease_written", "uri", written.URI, "cid", written.CID)
	return commonv1.StrongRef{URI: written.URI, CID: written.CID}, nil
}

func (r *contractRunner) deleteAgentPeer(ctx context.Context) {
	q := r.q
	if q.repo == nil || r.snapshot().Spec.Agent == nil {
		return
	}
	_, identity := r.agentIdentity(r.stateCopy())
	if identity == "" {
		return
	}
	deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := q.repo.DeleteRecord(deleteCtx, q.repoDID, AgentPeerNSID, identity); err != nil && !errors.Is(err, market.ErrNotFound) {
		r.log().Warn("requester_agent_peer_delete_failed", "identity", identity, "err", err.Error())
		return
	}
	r.log().Info("requester_agent_peer_deleted", "identity", identity)
}

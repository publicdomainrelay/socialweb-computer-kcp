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

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
)

const (
	AtircNickEnv = "ATIRC_NICK"

	AtircUsernameEnv = "ATP_USERNAME"

	agentPeerPublished = "PeerPublished"
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
	if fromGuest(contract) {
		ticket := clusterlink.StableTicket(clusterlink.Key{Private: ed25519.NewKeyFromSeed(seed)})
		entry := SecretEntry{Path: cloudinit.GuestRequesterAgentEnvFile, Value: "R2_COLOCATED=" + nick + "=" + ticket + "\n", Mode: "0600"}
		if held, found, err := r.bundleEntry(ctx, contract, entry.Path); err != nil {
			return err
		} else if !found || held != entry {
			if err := r.putBundleEntry(ctx, contract, entry); err != nil {
				return err
			}
			fresh = true
		}
	}
	r.agentPeerDone(ctx, contract, envFile, fresh, identity, "in the bundle")
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
	r.noteAgentMint(ctx, true, agentPeerPublished, "the atirc agent "+identity+"'s mesh key is "+how+"; the guest requester publishes its peer record as its co-located account "+nickOf(identity))
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

func nickOf(identity string) string {
	nick, _, _ := strings.Cut(identity, ".")
	return nick
}

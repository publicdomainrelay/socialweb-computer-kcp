package requester

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	DefaultAtircURL = "https://irc.fedfork.com"

	AtircServiceAudience = "did:web:irc.fedfork.com#atirc"

	AtircMintNSID = "chat.atirc.mint"

	AtircRevokeNSID = "chat.atirc.revoke"

	AtircTokenEnv = "ATIRC_TOKEN"

	AtircSubhandleEnv = "ATIRC_SUBHANDLE"

	AtircExpEnv = "ATIRC_EXP"

	AtircTokenLifetime = 24 * time.Hour

	AtircParentPath = "identity/atirc-parent"

	AtircMintedPath = "contracts/_atirc-minted"

	DefaultSecretsMount = "requester"

	atircReleaseKey = "atircReleasePending"

	atircMintedKey = "atircMinted"

	atircMintLeased = "Leased"

	atircMintPlain = "Unleased"

	atircMintPresent = "Present"

	atircCallTimeout = 30 * time.Second

	maxAtircLabel = 14

	maxAtircResponse = 64 << 10
)

type atircError struct {
	base    string
	status  int
	name    string
	message string
}

func (e *atircError) Error() string {
	return fmt.Sprintf("%s answered %d %s %s", e.base, e.status, e.name, e.message)
}

type atircMintInput struct {
	Subhandle string      `json:"subhandle"`
	Exp       int64       `json:"exp"`
	Lease     *atircLease `json:"lease,omitempty"`
}

type atircRef struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type atircLease struct {
	Association *atircRef `json:"association,omitempty"`

	Receipt atircRef `json:"receipt"`

	Termination string `json:"termination"`

	Contract string `json:"contract,omitempty"`
}

func (l *atircLease) equal(o *atircLease) bool {
	switch {
	case l == nil || o == nil:
		return l == o
	case (l.Association == nil) != (o.Association == nil):
		return false
	case l.Association != nil && *l.Association != *o.Association:
		return false
	}
	return l.Receipt == o.Receipt && l.Termination == o.Termination && l.Contract == o.Contract
}

func contractLease(state *v1alpha1.NodeClaimContract, association *commonv1.StrongRef) (*atircLease, error) {
	if state == nil || state.Receipt == nil || state.Receipt.URI == "" || state.Receipt.CID == "" || state.ReceiptOk == nil || !*state.ReceiptOk {
		return nil, errors.New("the contract has no verified receipt; no atirc token is minted before it")
	}
	bidder, _, _ := strings.Cut(strings.TrimPrefix(state.Receipt.URI, "at://"), "/")
	if !strings.HasPrefix(bidder, "did:") {
		return nil, fmt.Errorf("the receipt %s is not in a DID repo", state.Receipt.URI)
	}
	rkey, _ := market.EventRKeys(market.TerminationKey(state.Receipt.URI))
	lease := &atircLease{
		Receipt:     atircRef{URI: state.Receipt.URI, CID: state.Receipt.CID},
		Termination: "at://" + bidder + "/" + market.ComputeEventsVMTerminatedNSID + "/" + rkey,
	}
	if state.Accept != nil {
		lease.Contract = state.Accept.URI
	}
	if association != nil && association.URI != "" && association.CID != "" {
		lease.Association = &atircRef{URI: association.URI, CID: association.CID}
	}
	return lease, nil
}

func atircTokenLeases(token string) ([]atircLease, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims struct {
		Leases []atircLease `json:"leases"`
	}
	if json.Unmarshal(raw, &claims) != nil || len(claims.Leases) == 0 {
		return nil, false
	}
	return claims.Leases, true
}

func checkLeaseChain(got, parent []atircLease, own *atircLease) error {
	if len(got) != len(parent)+1 {
		return fmt.Errorf("the token carries %d leases, not the %d of its parent and its own", len(got), len(parent)+1)
	}
	for i := range parent {
		if !got[i].equal(&parent[i]) {
			return fmt.Errorf("the token's lease %d is not its parent's", i)
		}
	}
	if !got[len(parent)].equal(own) {
		return errors.New("the token's own lease is not the one minted")
	}
	return nil
}

var errAtircLeaseMissing = errors.New("the atirc endpoint answered a leased mint with a token that carries no leases claim")

func agentMintFailReason(err error) string {
	if errors.Is(err, errAtircLeaseMissing) {
		return "LeaseClaimMissing"
	}
	return "MintFailed"
}

func agentMintFailMessage(attempt int, err error) string {
	if errors.Is(err, errAtircLeaseMissing) {
		return fmt.Sprintf("attempt %d, not retried: %s", attempt, err.Error())
	}
	return fmt.Sprintf("attempt %d of %d: %s", attempt, associationRetryAttempts, err.Error())
}

func liveAtircEntry(entries []SecretEntry, path string, now time.Time) bool {
	for _, e := range entries {
		if e.Path != path {
			continue
		}
		if token := atircToken(e.Value); token != "" {
			if exp, ok := atircTokenExp(token, e.Value); ok && now.Before(exp) {
				return true
			}
		}
	}
	return false
}

type atircRevokeInput struct {
	Subhandle string `json:"subhandle"`
}

type atircIssuer struct {
	bearer string

	base string

	label string

	exp time.Time

	parent bool

	leases []atircLease
}

func (r *contractRunner) atircIssuer(ctx context.Context, contract v1alpha1.ComputeContract) (atircIssuer, error) {
	q := r.q
	if ref := q.cfg.Spec.AtircParentRef; ref != nil {
		if contract.Spec.SecretsRef == nil || ref.Mount != contract.Spec.SecretsRef.Mount || ref.Path != AtircParentPath ||
			!sameVault(ref.Vault, contract.Spec.SecretsRef.Vault) {
			return atircIssuer{}, fmt.Errorf("spec.atircParentRef %s/%s is not %s in the requester's own mount", ref.Mount, ref.Path, AtircParentPath)
		}
		if q.deps.Vault == nil {
			return atircIssuer{}, errors.New("the requester names an atirc parent token and no vault")
		}
		kv, err := q.deps.Vault(ctx, *ref)
		if err != nil {
			return atircIssuer{}, err
		}
		data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
		if err != nil {
			return atircIssuer{}, fmt.Errorf("reading the atirc parent token: %w", err)
		}
		token, _ := data["token"].(string)
		subhandle, _ := data["subhandle"].(string)
		token, subhandle = strings.TrimSpace(token), strings.ToLower(strings.TrimSpace(subhandle))
		if token == "" || subhandle == "" || strings.ContainsAny(token, " \t\r\n") {
			return atircIssuer{}, errors.New("the atirc parent token secret holds no token and subhandle")
		}
		exp, ok := atircTokenExp(token, "")
		if !ok {
			if raw, _ := data["exp"].(string); raw != "" {
				if unix, err := strconv.ParseInt(raw, 10, 64); err == nil && unix > 0 {
					exp, ok = time.Unix(unix, 0), true
				}
			}
		}
		if !ok {
			return atircIssuer{}, errors.New("the atirc parent token has no known expiry")
		}
		leases, _ := atircTokenLeases(token)
		return atircIssuer{bearer: token, base: subhandle, label: "vm", exp: exp, parent: true, leases: leases}, nil
	}
	handle := strings.ToLower(strings.TrimSpace(q.handle))
	switch {
	case handle == "":
		return atircIssuer{}, errors.New("the requester identity has no handle")
	case q.serviceAuth == nil:
		return atircIssuer{}, errors.New("the requester identity signs no service auth")
	}
	ctx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	bearer, err := q.serviceAuth.ServiceAuth(ctx, AtircServiceAudience, AtircMintNSID)
	if err != nil {
		return atircIssuer{}, fmt.Errorf("service auth for %s: %w", AtircServiceAudience, err)
	}
	label, _, _ := strings.Cut(handle, ".")
	if len(label) > maxAtircLabel {
		label = label[:maxAtircLabel]
	}
	return atircIssuer{bearer: bearer, base: handle, label: strings.TrimRight(label, "-")}, nil
}

func atircChild(label, rkey, base string) string {
	label, rkey = strings.TrimRight(label, "-"), strings.ToLower(rkey)
	if label == "" || rkey == "" || base == "" {
		return ""
	}
	return label + "-" + rkey + "." + base
}

func uriRKey(uri string) string {
	return uri[strings.LastIndex(uri, "/")+1:]
}

func (r *contractRunner) ensureAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract) {
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" || r.released() {
		return
	}
	rfpURI := ""
	if state != nil {
		rfpURI = state.RFPURI
	}
	reason := ""
	lease, err := contractLease(state, nil)
	if err == nil {
		reason, err = r.writeAtircToken(ctx, contract, envFile, "", rfpURI, time.Time{}, lease)
	}
	if err == nil {
		r.noteAgentMint(ctx, true, reason, "the atirc agent token is in the bundle ("+reason+")")
		return
	}
	r.log().Warn("requester_atirc_mint_failed", "envFile", envFile, "attempt", 1, "err", err.Error())
	r.noteAgentMint(ctx, false, agentMintFailReason(err), agentMintFailMessage(1, err))
	if lease == nil || errors.Is(err, errAtircLeaseMissing) {
		return
	}
	if _, retrying := r.q.atircMintRetrying.LoadOrStore(contract.Name, true); retrying {
		return
	}
	r.q.wg.Go(func() {
		defer r.q.atircMintRetrying.Delete(contract.Name)
		for attempt := 2; attempt <= associationRetryAttempts; attempt++ {
			if !sleepRetry(ctx, attempt-1) {
				return
			}
			current := r.snapshot()
			if current.Status.AtircRevokes != "" || r.released() {
				return
			}
			reason, err := r.writeAtircToken(ctx, current, envFile, "", rfpURI, time.Time{}, lease)
			if err == nil {
				r.log().Info("requester_atirc_minted_after_retry", "envFile", envFile, "attempt", attempt)
				r.noteAgentMint(ctx, true, reason, fmt.Sprintf("the atirc agent token is in the bundle (%s, attempt %d)", reason, attempt))
				return
			}
			r.log().Warn("requester_atirc_mint_failed", "envFile", envFile, "attempt", attempt, "err", err.Error())
			r.noteAgentMint(ctx, false, agentMintFailReason(err), agentMintFailMessage(attempt, err))
			if errors.Is(err, errAtircLeaseMissing) {
				return
			}
		}
		r.log().Warn("requester_atirc_mint_gave_up", "envFile", envFile, "attempts", associationRetryAttempts)
		r.noteAgentMint(ctx, false, "GaveUp", fmt.Sprintf("no atirc agent token after %d attempts", associationRetryAttempts))
	})
}

type atircMintOutcome struct {
	ok bool

	reason string

	message string
}

func (r *contractRunner) noteAgentMint(ctx context.Context, ok bool, reason, message string) {
	if r.isGone() {
		return
	}
	r.mu.Lock()
	r.atircMint = &atircMintOutcome{ok: ok, reason: reason, message: message}
	r.mu.Unlock()
	contract := r.snapshot()
	status := v1alpha1.ComputeContractStatus{Conditions: append([]metav1.Condition(nil), contract.Status.Conditions...)}
	conds.SetStatusAt(&status.Conditions, contract.Generation, v1alpha1.ComputeContractConditionAtircTokenMinted, conds.Status(ok), reason, message, r.q.now())
	if err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, status); err != nil && !notFound(err) {
		r.log().Warn("requester_atirc_mint_status_failed", "err", err.Error())
	}
}

func (r *contractRunner) agentMintOutcome() *atircMintOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.atircMint
}

func (r *contractRunner) ensureGuestRequesterToken(ctx context.Context, contract v1alpha1.ComputeContract, state *v1alpha1.NodeClaimContract, association commonv1.StrongRef, deadline time.Time) error {
	if !fromGuest(contract) || state == nil || state.Accept == nil {
		return nil
	}
	lease, err := contractLease(state, &association)
	if err != nil {
		return err
	}
	_, err = r.writeAtircToken(ctx, contract, cloudinit.GuestRequesterAtircEnvFile, "r2", state.Accept.URI, deadline, lease)
	return err
}

func (r *contractRunner) mintedEntry(ctx context.Context, contract v1alpha1.ComputeContract, entry SecretEntry, fresh bool) {
	r.addSecretEntries(entry)
	r.mu.Lock()
	secrets := r.secrets
	r.mu.Unlock()
	withheld := secrets.minted(entry.Path)
	if entry.Path == cloudinit.GuestRequesterAtircEnvFile || (!withheld && !fresh) {
		return
	}
	if err := r.notifySecretsUpdated(ctx, contract, commonv1.StrongRef{URI: entry.Path}); err != nil {
		r.log().Warn("requester_atirc_minted_notify_failed", "path", entry.Path, "err", err.Error())
	}
}

func (r *contractRunner) postMint(ctx context.Context, bearer string, in atircMintInput) (string, error) {
	mintCtx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	var out struct {
		Token string `json:"token"`
	}
	if err := r.postAtirc(mintCtx, AtircMintNSID, bearer, in, &out); err != nil {
		return "", fmt.Errorf("mint %s: %w", in.Subhandle, err)
	}
	token := strings.TrimSpace(out.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New(r.atircBase() + " answered no token")
	}
	return token, nil
}

func (r *contractRunner) mintAtirc(ctx context.Context, issuer atircIssuer, parent atircRevokeParent, in atircMintInput) (string, string, error) {
	token, err := r.postMint(ctx, issuer.bearer, in)
	if err != nil {
		return "", "", err
	}
	if in.Lease == nil {
		return token, atircMintPlain, nil
	}
	refuse := func(cause error) (string, string, error) {
		if _, revokeErr := r.revokeMinted(ctx, parent, token, in.Subhandle); revokeErr != nil {
			return "", "", &atircUnrevoked{token: token, subhandle: in.Subhandle,
				err: fmt.Errorf("mint %s: %w; its revoke failed: %w", in.Subhandle, cause, revokeErr)}
		}
		return "", "", fmt.Errorf("mint %s: %w", in.Subhandle, cause)
	}
	leases, ok := atircTokenLeases(token)
	if !ok {
		return refuse(fmt.Errorf("%w (%s)", errAtircLeaseMissing, r.atircBase()))
	}
	if err := checkLeaseChain(leases, issuer.leases, in.Lease); err != nil {
		return refuse(err)
	}
	return token, atircMintLeased, nil
}

func (r *contractRunner) writeAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, path, label, uri string, deadline time.Time, lease *atircLease) (string, error) {
	q := r.q
	if err := CheckTokenPaths(contract, nil); err != nil {
		return "", fmt.Errorf("no token is minted: %w", err)
	}
	switch {
	case q.deps.VaultWrite == nil:
		return "", errors.New("the requester has no vault write handle")
	case contract.Spec.SecretsRef == nil:
		return "", errors.New("the contract has no secrets bundle")
	case uri == "":
		return "", errors.New("the contract has no record to name the subhandle after yet")
	case contract.Status.AtircRevokes != "" || r.snapshot().Status.AtircRevokes != "":
		return "", errors.New("the contract's atirc tokens are revoked; none is minted again")
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("read the bundle: %w", err)
	}
	marker, err := markerIn(data)
	if err != nil {
		return "", fmt.Errorf("no token is minted: %w", err)
	}
	if marker.ownedBy(string(contract.UID)) || r.released() || q.revokesStarted(contract) {
		return "", errors.New("the contract's release has started; no atirc token is minted")
	}
	current := SecretEntry{Path: path, Mode: "0600"}
	at := -1
	for i, e := range entries {
		if e.Path == path {
			current, at = e, i
		}
	}
	if token := atircToken(current.Value); token != "" {
		if exp, ok := atircTokenExp(token, current.Value); ok && q.now().Before(exp) {
			r.mintedEntry(ctx, contract, current, false)
			return atircMintPresent, nil
		}
	}
	issuer, err := r.atircIssuer(ctx, contract)
	if err != nil {
		return "", err
	}
	if label == "" {
		label = issuer.label
	}
	subhandle := atircChild(label, uriRKey(uri), issuer.base)
	if subhandle == "" {
		return "", fmt.Errorf("%q and %q make no subhandle", issuer.base, uri)
	}
	exp := q.now().Add(AtircTokenLifetime)
	for _, bound := range []time.Time{deadline, issuer.exp} {
		if !bound.IsZero() && bound.Before(exp) {
			exp = bound
		}
	}
	if !exp.After(q.now()) {
		return "", fmt.Errorf("the token of %s would expire at %s", subhandle, exp.UTC().Format(time.RFC3339))
	}
	if err := q.indexMinted(ctx, contract, exp); err != nil {
		r.log().Warn("requester_atirc_index_failed", "subhandle", subhandle, "err", err.Error())
		return "", fmt.Errorf("no token is minted while the atirc minted index cannot record %s: %w", subhandle, err)
	}
	token, reason, err := r.mintAtirc(ctx, issuer, r.revokeParent(ctx, contract), atircMintInput{Subhandle: subhandle, Exp: exp.Unix(), Lease: lease})
	var unrevoked *atircUnrevoked
	if errors.As(err, &unrevoked) {
		r.recordUnrevoked(ctx, kv, ref, data, contract, path, exp, unrevoked)
	}
	if err != nil {
		return "", err
	}
	if claimed, ok := atircTokenExp(token, ""); ok {
		exp = claimed
	}
	current.Value = withAtircToken(current.Value, token, subhandle, exp)
	current.Mode = "0600"
	if at < 0 {
		entries = append(entries, current)
	} else {
		entries[at] = current
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	minted, err := withMinted(data, atircMinted{Path: path, Subhandle: subhandle, Token: token, Exp: exp.Unix(), UID: string(contract.UID)}, q.now())
	if err != nil {
		return "", err
	}
	next[atircMintedKey] = minted
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return "", fmt.Errorf("write the bundle: %w", err)
	}
	r.mintedEntry(ctx, contract, current, true)
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "path", path, "exp", exp.UTC().Format(time.RFC3339), "lease", reason)
	return reason, nil
}

type atircUnrevoked struct {
	token string

	subhandle string

	err error
}

func (e *atircUnrevoked) Error() string { return e.err.Error() }

func (e *atircUnrevoked) Unwrap() error { return e.err }

func (r *contractRunner) recordUnrevoked(ctx context.Context, kv vault.KV, ref commonv1.SecretRef, data map[string]any,
	contract v1alpha1.ComputeContract, path string, exp time.Time, u *atircUnrevoked) {
	if claimed, ok := atircTokenExp(u.token, ""); ok {
		exp = claimed
	}
	minted, err := withMinted(data, atircMinted{Path: path, Subhandle: u.subhandle, Token: u.token, Exp: exp.Unix(), UID: string(contract.UID)}, r.q.now())
	if err == nil {
		next := make(map[string]any, len(data)+1)
		for k, v := range data {
			next[k] = v
		}
		next[atircMintedKey] = minted
		err = kv.Write(ctx, ref.Mount, ref.Path, next)
	}
	if err != nil {
		r.log().Warn("requester_atirc_unrevoked_unrecorded", "subhandle", u.subhandle, "path", path, "err", err.Error())
		return
	}
	r.log().Warn("requester_atirc_unrevoked_recorded", "subhandle", u.subhandle, "path", path, "exp", exp.UTC().Format(time.RFC3339))
}

type atircMinted struct {
	Path string `json:"path"`

	Subhandle string `json:"subhandle"`

	Token string `json:"token"`

	Exp int64 `json:"exp"`

	UID string `json:"uid,omitempty"`

	Revoked int64 `json:"revoked,omitempty"`
}

func mintedOf(data map[string]any) ([]atircMinted, error) {
	text, _ := data[atircMintedKey].(string)
	if text == "" {
		return nil, nil
	}
	var out []atircMinted
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("parse the minted atirc tokens: %w", err)
	}
	return out, nil
}

func withMinted(data map[string]any, add atircMinted, now time.Time) (string, error) {
	minted, err := mintedOf(data)
	if err != nil {
		return "", err
	}
	kept := []atircMinted{add}
	for _, m := range minted {
		if m.Exp > now.Unix() && m != add {
			kept = append(kept, m)
		}
	}
	raw, err := json.Marshal(kept)
	return string(raw), err
}

func atircTokenExp(token, env string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 {
				return time.Unix(claims.Exp, 0), true
			}
		}
	}
	if exp, err := strconv.ParseInt(atircEnvValue(env, AtircExpEnv), 10, 64); err == nil && exp > 0 {
		return time.Unix(exp, 0), true
	}
	return time.Time{}, false
}

func releaseTokenPaths(contract v1alpha1.ComputeContract) []string {
	var paths []string
	if envFile, err := agentEnvFile(contract); err == nil && envFile != "" {
		paths = append(paths, envFile)
	}
	if fromGuest(contract) {
		paths = append([]string{cloudinit.GuestRequesterAtircEnvFile}, paths...)
	}
	return paths
}

func sameVault(a, b *commonv1.ObjectRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

type atircReleaseMarker struct {
	Paths []string `json:"paths"`

	StartedAt int64 `json:"startedAt"`

	UID string `json:"uid,omitempty"`
}

func markerIn(data map[string]any) (*atircReleaseMarker, error) {
	text, _ := data[atircReleaseKey].(string)
	if text == "" {
		return nil, nil
	}
	var marker atircReleaseMarker
	if err := json.Unmarshal([]byte(text), &marker); err != nil {
		return nil, fmt.Errorf("parse the release marker: %w", err)
	}
	return &marker, nil
}

func (m *atircReleaseMarker) ownedBy(uid string) bool {
	return m != nil && (m.UID == "" || uid == "" || m.UID == uid)
}

func ReleaseRevokesExpected(contract v1alpha1.ComputeContract) bool {
	return contract.Spec.SecretsRef != nil && len(releaseTokenPaths(contract)) > 0
}

func ReleaseWait(contract v1alpha1.ComputeContract, base time.Duration) time.Duration {
	wait := base
	if fromGuest(contract) {
		wait += GuestRequesterReleaseTimeout
	}
	if acceptedWithoutReceipt(contract.Status.Contract) {
		wait += DeniedAcceptReceiptAttempts * DefaultReceiptTimeout
	}
	return wait
}

func (q *Requester) revokesStarted(contract v1alpha1.ComputeContract) bool {
	_, started := q.atircRevokesStarted.Load(contract.Name + "/" + string(contract.UID))
	return started
}

func (q *Requester) markRevokesStarted(contract v1alpha1.ComputeContract) {
	q.atircRevokesStarted.Store(contract.Name+"/"+string(contract.UID), true)
}

func (r *contractRunner) revokeAtircToken(ctx context.Context) (v1alpha1.AtircRevokesState, func()) {
	contract := r.snapshot()
	r.q.markRevokesStarted(contract)
	if !ReleaseRevokesExpected(contract) || r.q.deps.VaultWrite == nil {
		return "", nil
	}
	if state := contract.Status.AtircRevokes; state == v1alpha1.AtircRevokesDone || state == v1alpha1.AtircRevokesAbandoned {
		return state, nil
	}
	paths := releaseTokenPaths(contract)
	marker := &atircReleaseMarker{Paths: paths, StartedAt: r.q.now().Unix(), UID: string(contract.UID)}
	first, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(len(paths)+1)*atircCallTimeout)
	defer cancel()
	if err := r.setReleaseMarker(first, contract, marker); err != nil {
		r.log().Warn("requester_atirc_release_marker_failed", "err", err.Error())
		return v1alpha1.AtircRevokesPending, func() { r.q.startReleaseRevokes(r, contract, marker, true) }
	}
	var left []string
	for _, path := range paths {
		if !r.revokeEnvToken(first, contract, path) {
			left = append(left, path)
		}
	}
	r.log().Info("requester_atirc_release_revoke", "attempt", 0, "left", len(left))
	if len(left) == 0 {
		if err := r.setReleaseMarker(first, contract, nil); err == nil {
			return v1alpha1.AtircRevokesDone, nil
		}
	}
	return v1alpha1.AtircRevokesPending, func() { r.q.startReleaseRevokes(r, contract, marker, false) }
}

func (r *contractRunner) holdsLeasedToken(ctx context.Context, contract v1alpha1.ComputeContract) bool {
	r.bundleMu.Lock()
	_, data, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	r.bundleMu.Unlock()
	if err != nil {
		r.log().Warn("requester_atirc_revoke_before_destroy_skipped", "step", "read the bundle", "err", err.Error())
		return false
	}
	minted, _ := mintedOf(data)
	for _, m := range minted {
		if _, leased := atircTokenLeases(m.Token); leased && r.q.now().Before(time.Unix(m.Exp, 0)) {
			return true
		}
	}
	paths := releaseTokenPaths(contract)
	for _, e := range entries {
		if _, leased := atircTokenLeases(atircToken(e.Value)); leased && slices.Contains(paths, e.Path) {
			return true
		}
	}
	return false
}

func (r *contractRunner) revokeBeforeDestroy(ctx context.Context) {
	contract := r.snapshot()
	state := contract.Status.AtircRevokes
	if !ReleaseRevokesExpected(contract) || r.q.deps.VaultWrite == nil || state == v1alpha1.AtircRevokesDone || state == v1alpha1.AtircRevokesAbandoned {
		return
	}
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	leased := r.holdsLeasedToken(read, contract)
	cancel()
	if !leased {
		return
	}
	var (
		start func()
		ran   bool
	)
	r.atircRevoke.Do(func() {
		ran = true
		state, start = r.revokeAtircToken(ctx)
	})
	if !ran || state == "" {
		return
	}
	r.log().Info("requester_atirc_revoke_before_destroy", "atircRevokes", state)
	r.mergeRevokes(context.WithoutCancel(ctx), state)
	if start != nil {
		start()
	}
}

func (r *contractRunner) mergeRevokes(ctx context.Context, state v1alpha1.AtircRevokesState) {
	if err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{AtircRevokes: state}); err != nil && !notFound(err) {
		r.q.log.Warn("requester_atirc_revokes_status_failed", "contract", r.name, "err", err.Error())
	}
}

func (r *contractRunner) setReleaseMarker(ctx context.Context, contract v1alpha1.ComputeContract, marker *atircReleaseMarker) error {
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, _, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return err
	}
	if marker == nil {
		current, err := markerIn(data)
		if err == nil && current != nil && !current.ownedBy(string(contract.UID)) {
			return nil
		}
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		if k != atircReleaseKey {
			next[k] = v
		}
	}
	if marker != nil {
		raw, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		next[atircReleaseKey] = string(raw)
	} else if _, had := data[atircReleaseKey]; !had {
		return nil
	}
	return kv.Write(ctx, ref.Mount, ref.Path, next)
}

func (r *contractRunner) releaseMarker(ctx context.Context, contract v1alpha1.ComputeContract) (*atircReleaseMarker, error) {
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	_, data, _, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		return nil, err
	}
	marker, err := markerIn(data)
	if err != nil || !marker.ownedBy(string(contract.UID)) {
		return nil, err
	}
	return marker, nil
}

func (q *Requester) startReleaseRevokes(r *contractRunner, contract v1alpha1.ComputeContract, marker *atircReleaseMarker, write bool) {
	if _, running := q.releaseRevoking.LoadOrStore(contract.Name, true); running {
		return
	}
	go func() {
		defer q.releaseRevoking.Delete(contract.Name)
		if write && !r.writeReleaseMarker(contract, marker) {
			return
		}
		r.releaseRevokes(contract, marker)
	}()
}

func (r *contractRunner) writeReleaseMarker(contract v1alpha1.ComputeContract, marker *atircReleaseMarker) bool {
	stop := r.q.runContext()
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(stop, atircCallTimeout)
		err := r.setReleaseMarker(ctx, contract, marker)
		cancel()
		if err == nil {
			return true
		}
		r.q.log.Warn("requester_atirc_release_marker_failed", "contract", r.name, "attempt", attempt, "err", err.Error())
		if !r.waitRetry(stop, attempt) {
			return false
		}
		if !r.q.now().Before(time.Unix(marker.StartedAt, 0).Add(AtircTokenLifetime)) {
			r.q.log.Warn("requester_atirc_release_revoke_gave_up", "contract", r.name, "step", "the release marker")
			return false
		}
	}
}

func (r *contractRunner) waitRetry(stop context.Context, attempt int) bool {
	timer := time.NewTimer(min(associationRetryFirst<<min(attempt-1, 10), associationRetryMax))
	defer timer.Stop()
	select {
	case <-stop.Done():
		r.q.log.Info("requester_atirc_release_revoke_left_for_restart", "contract", r.name)
		return false
	case <-timer.C:
		return true
	}
}

func (q *Requester) resumeReleaseRevokes(contract v1alpha1.ComputeContract) {
	if !ReleaseRevokesExpected(contract) || q.deps.VaultWrite == nil {
		return
	}
	q.markRevokesStarted(contract)
	if state := contract.Status.AtircRevokes; state == v1alpha1.AtircRevokesDone || state == v1alpha1.AtircRevokesAbandoned {
		return
	}
	if _, running := q.releaseRevoking.LoadOrStore(contract.Name, true); running {
		return
	}
	r := newContractRunner(q, contract)
	go func() {
		var marker *atircReleaseMarker
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(q.runContext(), atircCallTimeout)
			found, err := r.releaseMarker(ctx, contract)
			cancel()
			if err == nil {
				marker = found
				break
			}
			q.log.Warn("requester_atirc_release_marker_failed", "contract", contract.Name, "attempt", attempt, "err", err.Error())
			if attempt >= associationRetryAttempts || !r.waitRetry(q.runContext(), attempt) {
				q.releaseRevoking.Delete(contract.Name)
				return
			}
		}
		write := marker == nil || len(marker.Paths) == 0
		if write {
			marker = &atircReleaseMarker{Paths: releaseTokenPaths(contract), StartedAt: q.now().Unix(), UID: string(contract.UID)}
		}
		if contract.Status.AtircRevokes != v1alpha1.AtircRevokesPending {
			pending, cancelPending := context.WithTimeout(q.runContext(), atircCallTimeout)
			r.mergeRevokes(pending, v1alpha1.AtircRevokesPending)
			cancelPending()
		}
		q.releaseRevoking.Delete(contract.Name)
		q.log.Info("requester_atirc_release_revoke_resumed", "contract", contract.Name, "paths", marker.Paths, "rebuilt", write)
		q.startReleaseRevokes(r, contract, marker, write)
	}()
}

func (r *contractRunner) releaseRevokes(contract v1alpha1.ComputeContract, marker *atircReleaseMarker) {
	stop := r.q.runContext()
	done := map[string]bool{}
	giveUp := time.Unix(marker.StartedAt, 0).Add(AtircTokenLifetime)
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(stop, atircCallTimeout)
		left := 0
		for _, path := range marker.Paths {
			if !done[path] {
				done[path] = r.revokeEnvToken(ctx, contract, path)
			}
			if !done[path] {
				left++
			}
		}
		cleared := true
		if left == 0 {
			if err := r.setReleaseMarker(ctx, contract, nil); err != nil {
				cleared = false
				r.q.log.Warn("requester_atirc_release_marker_failed", "contract", r.name, "step", "clear", "err", err.Error())
			}
		}
		if left == 0 && cleared {
			r.mergeRevokes(ctx, v1alpha1.AtircRevokesDone)
		}
		cancel()
		r.q.log.Info("requester_atirc_release_revoke", "contract", r.name, "attempt", attempt, "left", left)
		if left == 0 && cleared {
			return
		}
		if !r.q.now().Before(giveUp) {
			r.q.log.Warn("requester_atirc_release_revoke_gave_up", "contract", r.name, "left", left)
			gone, cancelGone := context.WithTimeout(context.WithoutCancel(stop), atircCallTimeout)
			r.mergeRevokes(gone, v1alpha1.AtircRevokesAbandoned)
			cancelGone()
			return
		}
		if !r.waitRetry(stop, attempt) {
			return
		}
	}
}

func (r *contractRunner) revokeEnvToken(ctx context.Context, contract v1alpha1.ComputeContract, path string) bool {
	r.bundleMu.Lock()
	_, data, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	r.bundleMu.Unlock()
	if err != nil {
		r.q.log.Warn("requester_atirc_revoke_failed", "contract", r.name, "step", "read the bundle", "err", err.Error())
		return false
	}
	minted, err := mintedOf(data)
	if err != nil {
		r.q.log.Warn("requester_atirc_revoke_failed", "contract", r.name, "step", "read the minted tokens", "err", err.Error())
		return false
	}
	recorded, done := false, true
	parent := r.revokeParent(ctx, contract)
	var cleared []atircMinted
	for _, m := range minted {
		if m.Path != path {
			continue
		}
		recorded = true
		if m.Revoked != 0 || !r.q.now().Before(time.Unix(m.Exp, 0)) {
			continue
		}
		if _, err := r.revokeMinted(ctx, parent, m.Token, m.Subhandle); err != nil {
			done = false
			continue
		}
		cleared = append(cleared, m)
	}
	r.markMintedRevoked(ctx, *contract.Spec.SecretsRef, cleared)
	if recorded {
		return done
	}
	token, subhandle, env := "", "", ""
	for _, e := range entries {
		if e.Path == path {
			token, subhandle, env = atircToken(e.Value), atircEnvSubhandle(e.Value), e.Value
		}
	}
	if token == "" {
		return true
	}
	derived := subhandle == ""
	if derived {
		subhandle = r.legacySubhandle(contract, path)
	}
	if subhandle == "" {
		r.q.log.Warn("requester_atirc_revoke_failed", "contract", r.name, "path", path, "step", "the token names no subhandle and none can be derived")
		return false
	}
	if exp, ok := atircTokenExp(token, env); ok && !r.q.now().Before(exp) {
		return true
	}
	revoked, err := r.revokeAtircSubhandle(ctx, token, subhandle, false)
	if err != nil {
		return false
	}
	if !revoked && derived {
		r.q.log.Warn("requester_atirc_revoke_unconfirmed", "contract", r.name, "path", path, "subhandle", subhandle,
			"reason", "a derived subhandle answered 401; the token stays pending until it expires")
		return false
	}
	return true
}

func (r *contractRunner) markMintedRevoked(ctx context.Context, ref commonv1.SecretRef, cleared []atircMinted) {
	if len(cleared) == 0 {
		return
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	kv, data, _, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		r.log().Warn("requester_atirc_revoked_unmarked", "step", "read the bundle", "err", err.Error())
		return
	}
	minted, err := mintedOf(data)
	if err != nil {
		r.log().Warn("requester_atirc_revoked_unmarked", "step", "read the minted tokens", "err", err.Error())
		return
	}
	marked := 0
	for i, m := range minted {
		for _, c := range cleared {
			if m.Revoked == 0 && m.Token == c.Token && m.Subhandle == c.Subhandle {
				minted[i].Revoked = r.q.now().Unix()
				marked++
			}
		}
	}
	if marked == 0 {
		return
	}
	raw, err := json.Marshal(minted)
	if err != nil {
		return
	}
	next := make(map[string]any, len(data))
	for k, v := range data {
		next[k] = v
	}
	next[atircMintedKey] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		r.log().Warn("requester_atirc_revoked_unmarked", "step", "write the bundle", "err", err.Error())
	}
}

func (r *contractRunner) legacySubhandle(contract v1alpha1.ComputeContract, path string) string {
	handle := strings.ToLower(strings.TrimSpace(r.q.handle))
	state := contract.Status.Contract
	if handle == "" || state == nil {
		return ""
	}
	if path == cloudinit.GuestRequesterAtircEnvFile {
		if state.Accept == nil {
			return ""
		}
		return atircChild("r2", uriRKey(state.Accept.URI), handle)
	}
	label, _, _ := strings.Cut(handle, ".")
	if len(label) > maxAtircLabel {
		label = label[:maxAtircLabel]
	}
	return atircChild(label, uriRKey(state.RFPURI), handle)
}

type atircRevokeParent func() (atircIssuer, bool)

func (r *contractRunner) revokeParent(ctx context.Context, contract v1alpha1.ComputeContract) atircRevokeParent {
	if r.q.cfg.Spec.AtircParentRef == nil {
		return nil
	}
	var (
		issuer atircIssuer
		ok     bool
		read   bool
	)
	return func() (atircIssuer, bool) {
		if read {
			return issuer, ok
		}
		read = true
		found, err := r.atircIssuer(ctx, contract)
		switch {
		case err != nil:
			r.log().Warn("requester_atirc_revoke_parent_unavailable", "err", err.Error())
		case !r.q.now().Before(found.exp):
			r.log().Warn("requester_atirc_revoke_parent_unavailable", "subhandle", found.base, "exp", found.exp.UTC().Format(time.RFC3339))
		default:
			issuer, ok = found, true
		}
		return issuer, ok
	}
}

func atircBelow(subhandle, base string) bool {
	return base != "" && strings.HasSuffix(subhandle, "."+base)
}

func atircTokenSubhandle(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Subhandle string `json:"subhandle"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Subhandle
}

func (r *contractRunner) revokeMinted(ctx context.Context, parent atircRevokeParent, token, subhandle string) (bool, error) {
	if parent != nil {
		if issuer, ok := parent(); ok && atircBelow(subhandle, issuer.base) && atircTokenSubhandle(token) == subhandle {
			revoked, err := r.revokeAtircSubhandle(ctx, issuer.bearer, subhandle, true)
			var xerr *atircError
			if err == nil || !errors.As(err, &xerr) || (xerr.status != http.StatusUnauthorized && xerr.status != http.StatusForbidden) {
				return revoked, err
			}
			r.log().Warn("requester_atirc_revoke_parent_refused", "subhandle", subhandle, "parent", issuer.base, "err", err.Error())
		}
	}
	return r.revokeAtircSubhandle(ctx, token, subhandle, false)
}

func atircLeaseDead(e *atircError) bool {
	return strings.Contains(strings.ToLower(e.name+" "+e.message), "lease is not alive")
}

func (r *contractRunner) revokeAtircSubhandle(ctx context.Context, bearer, subhandle string, byParent bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	defer cancel()
	var out struct {
		RevokedAt int64 `json:"revokedAt"`
	}
	err := r.postAtirc(ctx, AtircRevokeNSID, bearer, atircRevokeInput{Subhandle: subhandle}, &out)
	var xerr *atircError
	switch {
	case err != nil && byParent:
		return false, err
	case errors.As(err, &xerr) && xerr.status == http.StatusUnauthorized && atircLeaseDead(xerr):
		r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "byLease", true)
		return true, nil
	case errors.As(err, &xerr) && xerr.status == http.StatusUnauthorized:
		r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "already", true)
		return false, nil
	case err != nil:
		r.log().Warn("requester_atirc_revoke_failed", "subhandle", subhandle, "step", "revoke", "err", err.Error())
		return false, err
	}
	r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "byParent", byParent, "revokedAt", time.Unix(out.RevokedAt, 0).UTC().Format(time.RFC3339))
	return true, nil
}

func (r *contractRunner) readAtircBundle(ctx context.Context, ref commonv1.SecretRef) (vault.KV, map[string]any, []SecretEntry, error) {
	kv, err := r.q.deps.VaultWrite(ctx, ref)
	if err != nil {
		return nil, nil, nil, err
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	text, _ := data["bundle"].(string)
	entries := []SecretEntry{}
	if text != "" {
		if err := json.Unmarshal([]byte(text), &entries); err != nil {
			return nil, nil, nil, fmt.Errorf("parse the bundle: %w", err)
		}
	}
	return kv, data, entries, nil
}

func (r *contractRunner) atircBase() string {
	return strings.TrimRight(cmp.Or(strings.TrimSpace(r.q.cfg.AtircURL), DefaultAtircURL), "/")
}

func (r *contractRunner) postAtirc(ctx context.Context, nsid, bearer string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	base := r.atircBase()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/xrpc/"+nsid, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	client := r.q.deps.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAtircResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var xerr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &xerr)
		return &atircError{base: base, status: resp.StatusCode, name: xerr.Error, message: xerr.Message}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s answered no JSON: %w", base, err)
	}
	return nil
}

func atircEnvValue(env, name string) string {
	for line := range strings.Lines(env) {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), name+"=")
		if value = strings.TrimSpace(value); ok && value != "" {
			return value
		}
	}
	return ""
}

func atircToken(env string) string {
	return atircEnvValue(env, AtircTokenEnv)
}

func atircEnvSubhandle(env string) string {
	return atircEnvValue(env, AtircSubhandleEnv)
}

func withoutAtircToken(env string) string {
	var b strings.Builder
	for line := range strings.Lines(env) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, AtircTokenEnv+"=") || strings.HasPrefix(trimmed, AtircSubhandleEnv+"=") || strings.HasPrefix(trimmed, AtircExpEnv+"=") {
			continue
		}
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func withAtircToken(env, token, subhandle string, exp time.Time) string {
	var b strings.Builder
	b.WriteString(withoutAtircToken(env))
	b.WriteString(AtircExpEnv + "=" + strconv.FormatInt(exp.Unix(), 10) + "\n")
	b.WriteString(AtircTokenEnv + "=" + token + "\n")
	b.WriteString(AtircSubhandleEnv + "=" + subhandle + "\n")
	return b.String()
}

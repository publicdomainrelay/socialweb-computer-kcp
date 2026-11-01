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
	"strconv"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

const (
	DefaultAtircURL = "https://irc.fedfork.com"

	AtircServiceAudience = "did:web:irc.fedfork.com#atirc"

	AtircMintNSID = "chat.atirc.mint"

	AtircRevokeNSID = "chat.atirc.revoke"

	AtircTokenEnv = "ATIRC_TOKEN"

	AtircSubhandleEnv = "ATIRC_SUBHANDLE"

	AtircAssocEnv = "ATIRC_ASSOC"

	AtircExpEnv = "ATIRC_EXP"

	AtircTokenLifetime = 24 * time.Hour

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
	Subhandle string              `json:"subhandle"`
	Exp       int64               `json:"exp"`
	Assoc     *commonv1.StrongRef `json:"assoc,omitempty"`
}

type atircRevokeInput struct {
	Subhandle string `json:"subhandle"`
}

const atircRevokeKey = "atircRevoke"

type atircPendingRevoke struct {
	Subhandle string `json:"subhandle"`

	Token string `json:"token"`

	Exp int64 `json:"exp"`
}

func (r *contractRunner) ensureAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string) {
	if contract.Spec.Association != nil && !fromGuest(contract) {
		if envFile, err := agentEnvFile(contract); err == nil && envFile != "" {
			r.log().Info("requester_atirc_mint_skipped", "reason", "the token is minted bound to the contract's association record")
		}
		return
	}
	if err := r.writeAtircToken(ctx, contract, rfpURI, nil, time.Time{}); err != nil {
		r.log().Warn("requester_atirc_mint_failed", "err", err.Error())
	}
}

func (r *contractRunner) bindAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, assoc commonv1.StrongRef, expiresAt time.Time) error {
	if envFile, err := agentEnvFile(contract); err != nil || envFile == "" {
		return err
	}
	state := r.stateCopy()
	if state == nil || state.RFPURI == "" {
		return errors.New("the contract has no RFP yet")
	}
	r.mu.Lock()
	bound := r.atircAssoc
	r.mu.Unlock()
	if bound == assoc {
		return nil
	}
	if err := r.writeAtircToken(ctx, contract, state.RFPURI, &assoc, expiresAt); err != nil {
		return err
	}
	r.mu.Lock()
	r.atircAssoc = assoc
	r.mu.Unlock()
	return nil
}

func (r *contractRunner) writeAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string, assoc *commonv1.StrongRef, deadline time.Time) error {
	q := r.q
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return err
	}
	switch {
	case strings.TrimSpace(q.handle) == "":
		return errors.New("the requester identity has no handle")
	case q.serviceAuth == nil:
		return errors.New("the requester identity signs no service auth")
	case q.deps.VaultWrite == nil:
		return errors.New("the requester has no vault write handle")
	case rfpURI == "":
		return errors.New("the contract has no RFP yet")
	}
	subhandle := atircSubhandle(q.handle, rfpURI)
	if assoc != nil && fromGuest(contract) {
		subhandle = atircBoundSubhandle(q.handle, rfpURI)
	}
	if subhandle == "" {
		return fmt.Errorf("handle %q and RFP %q make no subhandle", q.handle, rfpURI)
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return fmt.Errorf("read the bundle: %w", err)
	}
	current := SecretEntry{Path: envFile, Mode: "0600"}
	at := -1
	for i, e := range entries {
		if e.Path == envFile {
			current, at = e, i
		}
	}
	previousToken, previousSubhandle := atircToken(current.Value), atircEnvSubhandle(current.Value)
	if previousToken != "" && previousSubhandle == "" {
		previousSubhandle = atircSubhandle(q.handle, rfpURI)
	}
	if assoc == nil && previousToken != "" {
		return nil
	}
	if previousToken != "" && previousSubhandle == subhandle && atircEnvValue(current.Value, AtircAssocEnv) == assocValue(assoc) {
		return nil
	}
	exp := q.now().Add(AtircTokenLifetime)
	if !deadline.IsZero() && deadline.Before(exp) {
		exp = deadline
	}
	if !exp.After(q.now()) {
		return fmt.Errorf("the association expires at %s", exp.UTC().Format(time.RFC3339))
	}
	next := make(map[string]any, len(data)+2)
	for k, v := range data {
		next[k] = v
	}
	revoke := previousToken != "" && previousSubhandle != subhandle
	if revoke {
		previousExp, ok := atircTokenExp(previousToken, current.Value)
		if !ok {
			return fmt.Errorf("the expiry of the token of %s is unknown; its revoke cannot be bounded", previousSubhandle)
		}
		pending, err := json.Marshal(atircPendingRevoke{Subhandle: previousSubhandle, Token: previousToken, Exp: previousExp.Unix()})
		if err != nil {
			return err
		}
		next[atircRevokeKey] = string(pending)
	}
	token, err := r.mintAtircToken(ctx, subhandle, exp, assoc)
	if err != nil {
		return fmt.Errorf("mint: %w", err)
	}
	if revoke {
		if err := q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{AtircRevoke: "pending " + previousSubhandle}); err != nil && !notFound(err) {
			return fmt.Errorf("record the pending revoke of %s: %w", previousSubhandle, err)
		}
	}
	current.Value = withAtircToken(current.Value, token, subhandle, assocValue(assoc), exp)
	current.Mode = "0600"
	if at < 0 {
		entries = append(entries, current)
	} else {
		entries[at] = current
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return fmt.Errorf("write the bundle: %w", err)
	}
	r.addSecretEntries(current)
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "envFile", envFile, "exp", exp.UTC().Format(time.RFC3339), "assoc", assocValue(assoc))
	if revoke {
		r.mu.Lock()
		r.revokeOutstanding = true
		r.mu.Unlock()
		r.kickAssociation()
	}
	return nil
}

func assocValue(assoc *commonv1.StrongRef) string {
	if assoc == nil {
		return ""
	}
	return assoc.URI + "#" + assoc.CID
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

func (r *contractRunner) revokePending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revokeOutstanding
}

func (r *contractRunner) retryPendingRevoke(ctx context.Context) {
	if !r.revokePendingOnce(ctx) {
		r.scheduleRevokeRetry()
	}
}

func (r *contractRunner) revokePendingOnce(ctx context.Context) bool {
	contract := r.snapshot()
	if contract.Spec.SecretsRef == nil || r.q.deps.VaultWrite == nil {
		return true
	}
	if envFile, err := agentEnvFile(contract); err != nil || envFile == "" {
		return true
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, _, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		r.log().Warn("requester_atirc_revoke_failed", "step", "read the bundle", "err", err.Error())
		return false
	}
	text, _ := data[atircRevokeKey].(string)
	if text == "" {
		r.mu.Lock()
		r.revokeOutstanding = false
		r.mu.Unlock()
		return true
	}
	var pending atircPendingRevoke
	_ = json.Unmarshal([]byte(text), &pending)
	outcome := ""
	switch {
	case pending.Subhandle == "" || pending.Token == "":
		outcome = "dropped an unreadable pending revoke"
	case !r.q.now().Before(time.Unix(pending.Exp, 0)):
		outcome = "expired " + pending.Subhandle
	case r.revokeAtircSubhandle(ctx, pending.Token, pending.Subhandle) == nil:
		outcome = "revoked " + pending.Subhandle
	default:
		r.mu.Lock()
		r.revokeOutstanding = true
		r.mu.Unlock()
		return false
	}
	next := make(map[string]any, len(data))
	for k, v := range data {
		if k != atircRevokeKey {
			next[k] = v
		}
	}
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		r.log().Warn("requester_atirc_revoke_failed", "step", "clear the pending revoke", "err", err.Error())
		return false
	}
	r.mu.Lock()
	r.revokeOutstanding, r.revokeAttempts = false, 0
	r.mu.Unlock()
	if err := r.q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{AtircRevoke: outcome}); err != nil && !notFound(err) {
		r.log().Warn("requester_atirc_revoke_status_failed", "err", err.Error())
	}
	return true
}

func (r *contractRunner) scheduleRevokeRetry() {
	r.mu.Lock()
	defer r.mu.Unlock()
	delay := min(associationRetryFirst<<min(r.revokeAttempts, 10), associationRetryMax)
	r.revokeAttempts++
	if r.revokeRetry != nil {
		r.revokeRetry.Stop()
	}
	r.revokeRetry = time.AfterFunc(delay, r.kickAssociation)
}

func (r *contractRunner) abandonUnboundToken(ctx context.Context, contract v1alpha1.ComputeContract) {
	q := r.q
	envFile, err := agentEnvFile(contract)
	state := r.stateCopy()
	if err != nil || envFile == "" || state == nil || state.RFPURI == "" || q.deps.VaultWrite == nil {
		return
	}
	fail := func(step string, err error) {
		r.log().Error("requester_atirc_abandon_failed", "step", step, "err", err.Error())
	}
	bound := atircBoundSubhandle(q.handle, state.RFPURI)
	r.bundleMu.Lock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		r.bundleMu.Unlock()
		fail("read the bundle", err)
		return
	}
	at, token, subhandle := -1, "", ""
	for i, e := range entries {
		if e.Path == envFile {
			at, token, subhandle = i, atircToken(e.Value), atircEnvSubhandle(e.Value)
		}
	}
	if subhandle == "" {
		subhandle = atircSubhandle(q.handle, state.RFPURI)
	}
	if at < 0 || token == "" || subhandle == bound {
		r.bundleMu.Unlock()
		return
	}
	exp, ok := atircTokenExp(token, entries[at].Value)
	if !ok {
		exp = q.now().Add(AtircTokenLifetime)
	}
	pending, err := json.Marshal(atircPendingRevoke{Subhandle: subhandle, Token: token, Exp: exp.Unix()})
	if err != nil {
		r.bundleMu.Unlock()
		fail("encode the pending revoke", err)
		return
	}
	entries[at].Value = withoutAtircToken(entries[at].Value)
	raw, err := json.Marshal(entries)
	if err != nil {
		r.bundleMu.Unlock()
		fail("encode the bundle", err)
		return
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"], next[atircRevokeKey] = string(raw), string(pending)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		r.bundleMu.Unlock()
		fail("write the bundle", err)
		return
	}
	r.addSecretEntries(entries[at])
	r.mu.Lock()
	r.revokeOutstanding = true
	r.mu.Unlock()
	r.bundleMu.Unlock()
	if err := q.deps.Intents.MergeComputeContractStatus(ctx, r.name, v1alpha1.ComputeContractStatus{AtircRevoke: "pending " + subhandle + " (no bound token could be minted)"}); err != nil && !notFound(err) {
		r.log().Warn("requester_atirc_revoke_status_failed", "err", err.Error())
	}
	r.log().Warn("requester_atirc_unbound_abandoned", "subhandle", subhandle)
	r.retryPendingRevoke(ctx)
}

func (r *contractRunner) revokeAtircToken(ctx context.Context) {
	contract := r.snapshot()
	state := r.stateCopy()
	if envFile, err := agentEnvFile(contract); err != nil || envFile == "" || state == nil || state.RFPURI == "" || r.q.deps.VaultWrite == nil {
		return
	}
	go r.releaseRevokes(contract, state.RFPURI)
}

func (r *contractRunner) releaseRevokes(contract v1alpha1.ComputeContract, rfpURI string) {
	pendingDone, currentDone := false, false
	for attempt := 1; attempt <= associationRetryAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), atircCallTimeout)
		if !pendingDone {
			pendingDone = r.revokePendingOnce(ctx)
		}
		if !currentDone {
			currentDone = r.revokeCurrentToken(ctx, contract, rfpURI)
		}
		cancel()
		r.q.log.Info("requester_atirc_release_revoke", "contract", r.name, "attempt", attempt, "pendingDone", pendingDone, "currentDone", currentDone)
		if pendingDone && currentDone {
			return
		}
		time.Sleep(min(associationRetryFirst<<min(attempt-1, 10), associationRetryMax))
	}
	r.q.log.Warn("requester_atirc_release_revoke_gave_up", "contract", r.name, "pendingDone", pendingDone, "currentDone", currentDone)
}

func (r *contractRunner) revokeCurrentToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string) bool {
	q := r.q
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return true
	}
	r.bundleMu.Lock()
	_, _, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	r.bundleMu.Unlock()
	if err != nil {
		r.q.log.Warn("requester_atirc_revoke_failed", "contract", r.name, "step", "read the bundle", "err", err.Error())
		return false
	}
	token, subhandle, env := "", "", ""
	for _, e := range entries {
		if e.Path == envFile {
			token, subhandle, env = atircToken(e.Value), atircEnvSubhandle(e.Value), e.Value
		}
	}
	if subhandle == "" {
		subhandle = atircSubhandle(q.handle, rfpURI)
	}
	if token == "" || subhandle == "" {
		return true
	}
	if exp, ok := atircTokenExp(token, env); ok && !q.now().Before(exp) {
		return true
	}
	return r.revokeAtircSubhandle(ctx, token, subhandle) == nil
}

func (r *contractRunner) revokeAtircSubhandle(ctx context.Context, token, subhandle string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	defer cancel()
	var out struct {
		RevokedAt int64 `json:"revokedAt"`
	}
	err := r.postAtirc(ctx, AtircRevokeNSID, token, atircRevokeInput{Subhandle: subhandle}, &out)
	var xerr *atircError
	switch {
	case errors.As(err, &xerr) && xerr.status == http.StatusUnauthorized:
		r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "already", true)
		return nil
	case err != nil:
		r.log().Warn("requester_atirc_revoke_failed", "subhandle", subhandle, "step", "revoke", "err", err.Error())
		return err
	}
	r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "revokedAt", time.Unix(out.RevokedAt, 0).UTC().Format(time.RFC3339))
	return nil
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

func (r *contractRunner) mintAtircToken(ctx context.Context, subhandle string, exp time.Time, assoc *commonv1.StrongRef) (string, error) {
	q := r.q
	ctx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	bearer, err := q.serviceAuth.ServiceAuth(ctx, AtircServiceAudience, AtircMintNSID)
	if err != nil {
		return "", fmt.Errorf("service auth for %s: %w", AtircServiceAudience, err)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := r.postAtirc(ctx, AtircMintNSID, bearer, atircMintInput{Subhandle: subhandle, Exp: exp.Unix(), Assoc: assoc}, &out); err != nil {
		return "", err
	}
	token := strings.TrimSpace(out.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New(r.atircBase() + " answered no token")
	}
	return token, nil
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

func atircSubhandle(handle, rfpURI string) string {
	handle = strings.ToLower(strings.TrimSpace(handle))
	rkey := rfpURI[strings.LastIndex(rfpURI, "/")+1:]
	label, _, _ := strings.Cut(handle, ".")
	if len(label) > maxAtircLabel {
		label = label[:maxAtircLabel]
	}
	label = strings.TrimRight(label, "-")
	if label == "" || rkey == "" || handle == "" {
		return ""
	}
	return label + "-" + strings.ToLower(rkey) + "." + handle
}

func atircBoundSubhandle(handle, rfpURI string) string {
	subhandle := atircSubhandle(handle, rfpURI)
	label, rest, ok := strings.Cut(subhandle, ".")
	if !ok || label == "" {
		return ""
	}
	return label + "-a." + rest
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
		if strings.HasPrefix(trimmed, AtircTokenEnv+"=") || strings.HasPrefix(trimmed, AtircSubhandleEnv+"=") ||
			strings.HasPrefix(trimmed, AtircAssocEnv+"=") || strings.HasPrefix(trimmed, AtircExpEnv+"=") {
			continue
		}
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func withAtircToken(env, token, subhandle, assoc string, exp time.Time) string {
	var b strings.Builder
	b.WriteString(withoutAtircToken(env))
	b.WriteString(AtircExpEnv + "=" + strconv.FormatInt(exp.Unix(), 10) + "\n")
	b.WriteString(AtircTokenEnv + "=" + token + "\n")
	b.WriteString(AtircSubhandleEnv + "=" + subhandle + "\n")
	if assoc != "" {
		b.WriteString(AtircAssocEnv + "=" + assoc + "\n")
	}
	return b.String()
}

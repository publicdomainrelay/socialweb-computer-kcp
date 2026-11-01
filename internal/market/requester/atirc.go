package requester

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

func (r *contractRunner) ensureAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string) {
	if contract.Spec.Association != nil && !fromGuest(contract) {
		if envFile, err := agentEnvFile(contract); err == nil && envFile != "" {
			r.log().Info("requester_atirc_mint_skipped", "reason", "the token is minted bound to the contract's association record")
		}
		return
	}
	r.writeAtircToken(ctx, contract, rfpURI, nil, time.Time{})
}

func (r *contractRunner) bindAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, assoc commonv1.StrongRef, expiresAt time.Time) {
	state := r.stateCopy()
	if state == nil || state.RFPURI == "" {
		return
	}
	r.mu.Lock()
	bound := r.atircAssoc
	r.mu.Unlock()
	if bound == assoc {
		return
	}
	if r.writeAtircToken(ctx, contract, state.RFPURI, &assoc, expiresAt) {
		r.mu.Lock()
		r.atircAssoc = assoc
		r.mu.Unlock()
	}
}

func (r *contractRunner) writeAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string, assoc *commonv1.StrongRef, deadline time.Time) bool {
	q := r.q
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return false
	}
	skip := func(reason string) bool {
		r.log().Info("requester_atirc_mint_skipped", "reason", reason)
		return false
	}
	switch {
	case strings.TrimSpace(q.handle) == "":
		return skip("the requester identity has no handle")
	case q.serviceAuth == nil:
		return skip("the requester identity signs no service auth")
	case q.deps.VaultWrite == nil:
		return skip("the requester has no vault write handle")
	case rfpURI == "":
		return skip("the contract has no RFP yet")
	}
	fail := func(step string, err error) bool {
		r.log().Warn("requester_atirc_mint_failed", "step", step, "err", err.Error())
		return false
	}
	subhandle := atircSubhandle(q.handle, rfpURI)
	if assoc != nil && fromGuest(contract) {
		subhandle = atircBoundSubhandle(q.handle, rfpURI)
	}
	if subhandle == "" {
		return fail("subhandle", fmt.Errorf("handle %q and RFP %q make no subhandle", q.handle, rfpURI))
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return fail("read the bundle", err)
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
		return true
	}
	exp := q.now().Add(AtircTokenLifetime)
	if !deadline.IsZero() && deadline.Before(exp) {
		exp = deadline
	}
	if !exp.After(q.now()) {
		return fail("exp", fmt.Errorf("the association expires at %s", exp.UTC().Format(time.RFC3339)))
	}
	token, err := r.mintAtircToken(ctx, subhandle, exp, assoc)
	if err != nil {
		return fail("mint", err)
	}
	current.Value = withAtircToken(current.Value, token, subhandle)
	current.Mode = "0600"
	if at < 0 {
		entries = append(entries, current)
	} else {
		entries[at] = current
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return fail("encode the bundle", err)
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return fail("write the bundle", err)
	}
	r.addSecretEntries(current)
	bound := ""
	if assoc != nil {
		bound = assoc.URI
	}
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "envFile", envFile, "exp", exp.UTC().Format(time.RFC3339), "assoc", bound)
	if previousToken != "" && previousSubhandle != subhandle {
		r.revokeAtircSubhandle(ctx, previousToken, previousSubhandle)
	}
	return true
}

func (r *contractRunner) revokeAtircToken(ctx context.Context) {
	q := r.q
	contract := r.snapshot()
	envFile, err := agentEnvFile(contract)
	state := r.stateCopy()
	if err != nil || envFile == "" || state == nil || state.RFPURI == "" || q.deps.VaultWrite == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	defer cancel()
	r.bundleMu.Lock()
	_, _, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	r.bundleMu.Unlock()
	if err != nil {
		r.log().Warn("requester_atirc_revoke_failed", "step", "read the bundle", "err", err.Error())
		return
	}
	token, subhandle := "", ""
	for _, e := range entries {
		if e.Path == envFile {
			token, subhandle = atircToken(e.Value), atircEnvSubhandle(e.Value)
		}
	}
	if subhandle == "" {
		subhandle = atircSubhandle(q.handle, state.RFPURI)
	}
	if token == "" || subhandle == "" {
		return
	}
	r.revokeAtircSubhandle(ctx, token, subhandle)
}

func (r *contractRunner) revokeAtircSubhandle(ctx context.Context, token, subhandle string) {
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
	case err != nil:
		r.log().Warn("requester_atirc_revoke_failed", "subhandle", subhandle, "step", "revoke", "err", err.Error())
	default:
		r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "revokedAt", time.Unix(out.RevokedAt, 0).UTC().Format(time.RFC3339))
	}
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

func withAtircToken(env, token, subhandle string) string {
	var b strings.Builder
	for line := range strings.Lines(env) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, AtircTokenEnv+"=") || strings.HasPrefix(trimmed, AtircSubhandleEnv+"=") {
			continue
		}
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString(AtircTokenEnv + "=" + token + "\n")
	b.WriteString(AtircSubhandleEnv + "=" + subhandle + "\n")
	return b.String()
}

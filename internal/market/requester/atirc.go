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
	Subhandle string `json:"subhandle"`
	Exp       int64  `json:"exp"`
}

type atircRevokeInput struct {
	Subhandle string `json:"subhandle"`
}

func (r *contractRunner) ensureAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string) {
	q := r.q
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return
	}
	skip := func(reason string) {
		r.log().Info("requester_atirc_mint_skipped", "reason", reason)
	}
	switch {
	case strings.TrimSpace(q.handle) == "":
		skip("the requester identity has no handle")
		return
	case q.serviceAuth == nil:
		skip("the requester identity signs no service auth")
		return
	case q.deps.VaultWrite == nil:
		skip("the requester has no vault write handle")
		return
	case rfpURI == "":
		skip("the contract has no RFP yet")
		return
	}
	fail := func(step string, err error) {
		r.log().Warn("requester_atirc_mint_failed", "step", step, "err", err.Error())
	}
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		fail("read the bundle", err)
		return
	}
	current := SecretEntry{Path: envFile, Mode: "0600"}
	at := -1
	for i, e := range entries {
		if e.Path == envFile {
			current, at = e, i
		}
	}
	if atircToken(current.Value) != "" {
		return
	}
	subhandle := atircSubhandle(q.handle, rfpURI)
	if subhandle == "" {
		fail("subhandle", fmt.Errorf("handle %q and RFP %q make no subhandle", q.handle, rfpURI))
		return
	}
	exp := q.now().Add(AtircTokenLifetime)
	token, err := r.mintAtircToken(ctx, subhandle, exp)
	if err != nil {
		fail("mint", err)
		return
	}
	current.Value = withAtircToken(current.Value, token)
	current.Mode = "0600"
	if at < 0 {
		entries = append(entries, current)
	} else {
		entries[at] = current
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		fail("encode the bundle", err)
		return
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		fail("write the bundle", err)
		return
	}
	r.mu.Lock()
	state := r.secrets
	r.mu.Unlock()
	if state != nil {
		state.addEntries(current)
	}
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "envFile", envFile, "exp", exp.UTC().Format(time.RFC3339))
}

func (r *contractRunner) revokeAtircToken(ctx context.Context) {
	q := r.q
	contract := r.snapshot()
	envFile, err := agentEnvFile(contract)
	state := r.stateCopy()
	if err != nil || envFile == "" || state == nil || state.RFPURI == "" || q.deps.VaultWrite == nil {
		return
	}
	subhandle := atircSubhandle(q.handle, state.RFPURI)
	if subhandle == "" {
		return
	}
	fail := func(step string, err error) {
		r.log().Warn("requester_atirc_revoke_failed", "subhandle", subhandle, "step", step, "err", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), atircCallTimeout)
	defer cancel()
	_, _, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	if err != nil {
		fail("read the bundle", err)
		return
	}
	token := ""
	for _, e := range entries {
		if e.Path == envFile {
			token = atircToken(e.Value)
		}
	}
	if token == "" {
		return
	}
	var out struct {
		RevokedAt int64 `json:"revokedAt"`
	}
	err = r.postAtirc(ctx, AtircRevokeNSID, token, atircRevokeInput{Subhandle: subhandle}, &out)
	var xerr *atircError
	switch {
	case errors.As(err, &xerr) && xerr.status == http.StatusUnauthorized:
		r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "already", true)
	case err != nil:
		fail("revoke", err)
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

func (r *contractRunner) mintAtircToken(ctx context.Context, subhandle string, exp time.Time) (string, error) {
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
	if err := r.postAtirc(ctx, AtircMintNSID, bearer, atircMintInput{Subhandle: subhandle, Exp: exp.Unix()}, &out); err != nil {
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

func atircToken(env string) string {
	for line := range strings.Lines(env) {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), AtircTokenEnv+"=")
		if value = strings.TrimSpace(value); ok && value != "" {
			return value
		}
	}
	return ""
}

func withAtircToken(env, token string) string {
	var b strings.Builder
	for line := range strings.Lines(env) {
		if strings.HasPrefix(strings.TrimSpace(line), AtircTokenEnv+"=") {
			continue
		}
		b.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString(AtircTokenEnv + "=" + token + "\n")
	return b.String()
}

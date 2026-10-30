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

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	DefaultAtircURL = "https://irc.fedfork.com"

	AtircServiceAudience = "did:web:irc.fedfork.com#atirc"

	AtircMintNSID = "chat.atirc.mint"

	AtircTokenEnv = "ATIRC_TOKEN"

	atircMintTimeout = 30 * time.Second

	maxAtircLabel = 14

	maxAtircResponse = 64 << 10
)

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
	kv, err := q.deps.VaultWrite(ctx, ref)
	if err != nil {
		fail("vault", err)
		return
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil {
		fail("read the bundle", err)
		return
	}
	text, _ := data["bundle"].(string)
	entries := []SecretEntry{}
	if text != "" {
		if err := json.Unmarshal([]byte(text), &entries); err != nil {
			fail("parse the bundle", err)
			return
		}
	}
	current := SecretEntry{Path: envFile, Mode: "0600"}
	at := -1
	for i, e := range entries {
		if e.Path == envFile {
			current, at = e, i
		}
	}
	if atircTokenSet(current.Value) {
		return
	}
	subhandle := atircSubhandle(q.handle, rfpURI)
	if subhandle == "" {
		fail("subhandle", fmt.Errorf("handle %q and RFP %q make no subhandle", q.handle, rfpURI))
		return
	}
	token, err := r.mintAtircToken(ctx, subhandle)
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
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "envFile", envFile)
}

func (r *contractRunner) mintAtircToken(ctx context.Context, subhandle string) (string, error) {
	q := r.q
	ctx, cancel := context.WithTimeout(ctx, atircMintTimeout)
	defer cancel()
	bearer, err := q.serviceAuth.ServiceAuth(ctx, AtircServiceAudience, AtircMintNSID)
	if err != nil {
		return "", fmt.Errorf("service auth for %s: %w", AtircServiceAudience, err)
	}
	body, err := json.Marshal(map[string]string{"subhandle": subhandle})
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(cmp.Or(strings.TrimSpace(q.cfg.AtircURL), DefaultAtircURL), "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/xrpc/"+AtircMintNSID, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	client := q.deps.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAtircResponse))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		var xerr struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &xerr)
		return "", fmt.Errorf("%s answered %d %s %s", base, resp.StatusCode, xerr.Error, xerr.Message)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("%s answered no JSON: %w", base, err)
	}
	token := strings.TrimSpace(out.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New(base + " answered no token")
	}
	return token, nil
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

func atircTokenSet(env string) bool {
	for line := range strings.Lines(env) {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), AtircTokenEnv+"=")
		if ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
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

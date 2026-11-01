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
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/cloudinit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
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

type atircIssuer struct {
	bearer string

	base string

	label string

	exp time.Time
}

func (r *contractRunner) atircIssuer(ctx context.Context) (atircIssuer, error) {
	q := r.q
	if ref := q.cfg.Spec.AtircParentRef; ref != nil {
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
		return atircIssuer{bearer: token, base: subhandle, label: "vm", exp: exp}, nil
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

func (r *contractRunner) ensureAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, rfpURI string) {
	envFile, err := agentEnvFile(contract)
	if err != nil || envFile == "" {
		return
	}
	if err := r.writeAtircToken(ctx, contract, envFile, "", rfpURI, time.Time{}); err != nil {
		r.log().Warn("requester_atirc_mint_failed", "envFile", envFile, "err", err.Error())
	}
}

func (r *contractRunner) ensureGuestRequesterToken(ctx context.Context, contract v1alpha1.ComputeContract, accept commonv1.StrongRef, deadline time.Time) error {
	if !fromGuest(contract) {
		return nil
	}
	return r.writeAtircToken(ctx, contract, cloudinit.GuestRequesterAtircEnvFile, "r2", accept.URI, deadline)
}

func (r *contractRunner) writeAtircToken(ctx context.Context, contract v1alpha1.ComputeContract, path, label, uri string, deadline time.Time) error {
	q := r.q
	switch {
	case q.deps.VaultWrite == nil:
		return errors.New("the requester has no vault write handle")
	case contract.Spec.SecretsRef == nil:
		return errors.New("the contract has no secrets bundle")
	case uri == "":
		return errors.New("the contract has no record to name the subhandle after yet")
	}
	r.bundleMu.Lock()
	defer r.bundleMu.Unlock()
	ref := *contract.Spec.SecretsRef
	kv, data, entries, err := r.readAtircBundle(ctx, ref)
	if err != nil {
		return fmt.Errorf("read the bundle: %w", err)
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
			return nil
		}
	}
	issuer, err := r.atircIssuer(ctx)
	if err != nil {
		return err
	}
	if label == "" {
		label = issuer.label
	}
	subhandle := atircChild(label, uriRKey(uri), issuer.base)
	if subhandle == "" {
		return fmt.Errorf("%q and %q make no subhandle", issuer.base, uri)
	}
	exp := q.now().Add(AtircTokenLifetime)
	for _, bound := range []time.Time{deadline, issuer.exp} {
		if !bound.IsZero() && bound.Before(exp) {
			exp = bound
		}
	}
	if !exp.After(q.now()) {
		return fmt.Errorf("the token of %s would expire at %s", subhandle, exp.UTC().Format(time.RFC3339))
	}
	mintCtx, cancel := context.WithTimeout(ctx, atircCallTimeout)
	defer cancel()
	var out struct {
		Token string `json:"token"`
	}
	if err := r.postAtirc(mintCtx, AtircMintNSID, issuer.bearer, atircMintInput{Subhandle: subhandle, Exp: exp.Unix()}, &out); err != nil {
		return fmt.Errorf("mint %s: %w", subhandle, err)
	}
	token := strings.TrimSpace(out.Token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New(r.atircBase() + " answered no token")
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
		return err
	}
	next := make(map[string]any, len(data)+1)
	for k, v := range data {
		next[k] = v
	}
	next["bundle"] = string(raw)
	if err := kv.Write(ctx, ref.Mount, ref.Path, next); err != nil {
		return fmt.Errorf("write the bundle: %w", err)
	}
	r.addSecretEntries(current)
	r.log().Info("requester_atirc_minted", "subhandle", subhandle, "path", path, "exp", exp.UTC().Format(time.RFC3339))
	return nil
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

func (r *contractRunner) revokeAtircToken(ctx context.Context) {
	contract := r.snapshot()
	if contract.Spec.SecretsRef == nil || r.q.deps.VaultWrite == nil {
		return
	}
	var paths []string
	if envFile, err := agentEnvFile(contract); err == nil && envFile != "" {
		paths = append(paths, envFile)
	}
	if fromGuest(contract) {
		paths = append(paths, cloudinit.GuestRequesterAtircEnvFile)
	}
	if len(paths) > 0 {
		go r.releaseRevokes(contract, paths)
	}
}

func (r *contractRunner) releaseRevokes(contract v1alpha1.ComputeContract, paths []string) {
	done := map[string]bool{}
	for attempt := 1; attempt <= associationRetryAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), atircCallTimeout)
		left := 0
		for _, path := range paths {
			if !done[path] {
				done[path] = r.revokeEnvToken(ctx, contract, path)
			}
			if !done[path] {
				left++
			}
		}
		cancel()
		r.q.log.Info("requester_atirc_release_revoke", "contract", r.name, "attempt", attempt, "left", left)
		if left == 0 {
			return
		}
		time.Sleep(min(associationRetryFirst<<min(attempt-1, 10), associationRetryMax))
	}
	r.q.log.Warn("requester_atirc_release_revoke_gave_up", "contract", r.name, "paths", paths)
}

func (r *contractRunner) revokeEnvToken(ctx context.Context, contract v1alpha1.ComputeContract, path string) bool {
	r.bundleMu.Lock()
	_, _, entries, err := r.readAtircBundle(ctx, *contract.Spec.SecretsRef)
	r.bundleMu.Unlock()
	if err != nil {
		r.q.log.Warn("requester_atirc_revoke_failed", "contract", r.name, "step", "read the bundle", "err", err.Error())
		return false
	}
	token, subhandle, env := "", "", ""
	for _, e := range entries {
		if e.Path == path {
			token, subhandle, env = atircToken(e.Value), atircEnvSubhandle(e.Value), e.Value
		}
	}
	if token == "" || subhandle == "" {
		return true
	}
	if exp, ok := atircTokenExp(token, env); ok && !r.q.now().Before(exp) {
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

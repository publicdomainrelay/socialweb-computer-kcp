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

	AtircParentPath = "identity/atirc-parent"

	atircReleaseKey = "atircReleasePending"

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
	issuer, err := r.atircIssuer(ctx, contract)
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
}

func (r *contractRunner) revokeAtircToken(ctx context.Context) v1alpha1.AtircRevokesState {
	contract := r.snapshot()
	paths := releaseTokenPaths(contract)
	if contract.Spec.SecretsRef == nil || r.q.deps.VaultWrite == nil || len(paths) == 0 {
		return ""
	}
	marker := &atircReleaseMarker{Paths: paths, StartedAt: r.q.now().Unix()}
	first, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(len(paths)+1)*atircCallTimeout)
	defer cancel()
	if err := r.setReleaseMarker(first, contract, marker); err != nil {
		r.log().Warn("requester_atirc_release_marker_failed", "err", err.Error())
		r.q.startReleaseRevokes(r, contract, marker, true)
		return v1alpha1.AtircRevokesPending
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
			return v1alpha1.AtircRevokesDone
		}
	}
	r.q.startReleaseRevokes(r, contract, marker, false)
	return v1alpha1.AtircRevokesPending
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
	text, _ := data[atircReleaseKey].(string)
	if text == "" {
		return nil, nil
	}
	var marker atircReleaseMarker
	if err := json.Unmarshal([]byte(text), &marker); err != nil {
		return nil, err
	}
	return &marker, nil
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
	if contract.Spec.SecretsRef == nil || q.deps.VaultWrite == nil || len(releaseTokenPaths(contract)) == 0 {
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
		q.releaseRevoking.Delete(contract.Name)
		if marker != nil && len(marker.Paths) > 0 {
			q.log.Info("requester_atirc_release_revoke_resumed", "contract", contract.Name, "paths", marker.Paths)
			q.startReleaseRevokes(r, contract, marker, false)
			return
		}
		if contract.Status.AtircRevokes == v1alpha1.AtircRevokesPending {
			done, cancelDone := context.WithTimeout(q.runContext(), atircCallTimeout)
			r.mergeRevokes(done, v1alpha1.AtircRevokesDone)
			cancelDone()
		}
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
	revoked, err := r.revokeAtircSubhandle(ctx, token, subhandle)
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

func (r *contractRunner) revokeAtircSubhandle(ctx context.Context, token, subhandle string) (bool, error) {
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
		return false, nil
	case err != nil:
		r.log().Warn("requester_atirc_revoke_failed", "subhandle", subhandle, "step", "revoke", "err", err.Error())
		return false, err
	}
	r.log().Info("requester_atirc_revoked", "subhandle", subhandle, "revokedAt", time.Unix(out.RevokedAt, 0).UTC().Format(time.RFC3339))
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

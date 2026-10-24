package openbao

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

var _ vault.AtprotoSessions = (*Handle)(nil)

type atprotoEnvelope struct {
	Data map[string]any `json:"data"`

	Errors []string `json:"errors"`
}

func (h *Handle) sessionTarget(ctx context.Context, mount, path string) (*Client, string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	path = strings.Trim(strings.TrimSpace(path), "/")
	did, ok := strings.CutPrefix(path, "session/")
	if !strings.HasPrefix(mount, "auth/") || !ok || did == "" || strings.Contains(did, "/") {
		return nil, "", fmt.Errorf("openbao: an atproto session is auth/<mount> and session/<did>, not %s and %s", mount, path)
	}
	target := mount + "/" + path
	if !h.allows(target) {
		return nil, "", fmt.Errorf("%w: %s", ErrOutOfScope, target)
	}
	client, err := h.clients(ctx, mount)
	if err != nil {
		return nil, "", err
	}
	return client, target, nil
}

func (h *Handle) Session(ctx context.Context, mount, path string) (vault.AtprotoSession, error) {
	client, target, err := h.sessionTarget(ctx, mount, path)
	if err != nil {
		return vault.AtprotoSession{}, err
	}
	status, raw, err := client.Raw(ctx, http.MethodGet, target, nil)
	if err != nil {
		return vault.AtprotoSession{}, err
	}
	data, err := atprotoAnswer(http.MethodGet, target, status, raw)
	if err != nil {
		return vault.AtprotoSession{}, err
	}
	s := vault.AtprotoSession{DID: answerString(data, "did"), Handle: answerString(data, "handle"), PDS: answerString(data, "pds"),
		Scope: answerString(data, "scope"), Status: answerString(data, "status"), StatusReason: answerString(data, "status_reason")}
	if at, perr := time.Parse(time.RFC3339, answerString(data, "expires_at")); perr == nil {
		s.ExpiresAt = at
	}
	if s.DID == "" {
		return vault.AtprotoSession{}, fmt.Errorf("%w: %s", vault.ErrNoAtprotoSession, target)
	}
	return s, nil
}

func (h *Handle) Proof(ctx context.Context, mount, path, method, target, nonce string) (vault.AtprotoProof, error) {
	client, session, err := h.sessionTarget(ctx, mount, path)
	if err != nil {
		return vault.AtprotoProof{}, err
	}
	body := map[string]any{"htm": method, "htu": target}
	if nonce = strings.TrimSpace(nonce); nonce != "" {
		body["nonce"] = nonce
	}
	status, raw, err := client.Raw(ctx, http.MethodPost, session+"/proof", body)
	if err != nil {
		return vault.AtprotoProof{}, err
	}
	data, err := atprotoAnswer(http.MethodPost, session+"/proof", status, raw)
	if err != nil {
		return vault.AtprotoProof{}, err
	}
	proof := vault.AtprotoProof{AccessToken: answerString(data, "access_token"), DPoP: answerString(data, "dpop_proof")}
	if proof.AccessToken == "" || proof.DPoP == "" {
		return vault.AtprotoProof{}, fmt.Errorf("openbao: %s/proof answered no access token or DPoP proof", session)
	}
	return proof, nil
}

func atprotoAnswer(method, path string, status int, raw []byte) (map[string]any, error) {
	var envelope atprotoEnvelope
	_ = json.Unmarshal(raw, &envelope)
	reason := strings.Join(envelope.Errors, "; ")
	switch {
	case status == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s", vault.ErrNoAtprotoSession, path)
	case status == http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", vault.ErrAtprotoSessionInactive, reason)
	case status < 200 || status > 299:
		return nil, &ResponseError{Method: method, Path: path, Status: status, Body: reason}
	case envelope.Data == nil:
		return nil, fmt.Errorf("%w: %s", vault.ErrNoAtprotoSession, path)
	}
	return envelope.Data, nil
}

func answerString(data map[string]any, key string) string {
	if s, ok := data[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

package guestcontrolplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

const (
	HealthPath = "/healthz"

	MaxGuestBody = 1 << 20
)

func (s *Service) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/{mount}/{step}", s.handleLogin)
	mux.HandleFunc("GET /v1/identity/oidc/token/{role}", s.handleOIDCToken)
	mux.HandleFunc("GET /v1/identity/oidc/.well-known/openid-configuration", s.handleDiscovery)
	mux.HandleFunc("GET /v1/identity/oidc/.well-known/keys", s.handleKeys)
	mux.HandleFunc("POST /v1/on-network", s.handleOnNetwork)
	mux.HandleFunc("GET "+HealthPath, func(w http.ResponseWriter, r *http.Request) {
		httpjson.Write(w, http.StatusOK, map[string]any{"ok": true})
	})
	return mux
}

func writeReply(w http.ResponseWriter, reply guestidentity.Reply) {
	status := reply.Status
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	body := reply.Body
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeGuestError(w http.ResponseWriter, status int, message string) {
	httpjson.Write(w, status, map[string]any{"errors": []string{message}})
}

func readJSONMap(r *http.Request) (map[string]any, error) {
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxGuestBody+1))
	value := map[string]any{}
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("the body is not a JSON object: %w", err)
	}
	return value, nil
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	mount := r.PathValue("mount")
	if mount != s.cfg.GuestLoginMount {
		writeGuestError(w, http.StatusNotFound, "no such mount")
		return
	}
	step := r.PathValue("step")
	if _, err := guestidentity.ParseLoginStep(step); err != nil {
		writeGuestError(w, http.StatusNotFound, "no such step")
		return
	}
	input, err := readJSONMap(r)
	if err != nil {
		writeGuestError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeReply(w, s.guest.LoginStep(r.Context(), step, input))
}

func (s *Service) handleOIDCToken(w http.ResponseWriter, r *http.Request) {
	if s.guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	role := r.PathValue("role")
	if !guestidentity.ValidRole(role) {
		writeGuestError(w, http.StatusNotFound, "no such role")
		return
	}
	writeReply(w, s.guest.OIDCToken(r.Context(), role, r.Header.Get(guestidentity.VaultTokenHeader)))
}

func (s *Service) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if s.guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	writeReply(w, s.guest.Discovery(r.Context()))
}

func (s *Service) handleKeys(w http.ResponseWriter, r *http.Request) {
	if s.guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	writeReply(w, s.guest.Keys(r.Context()))
}

func (s *Service) handleOnNetwork(w http.ResponseWriter, r *http.Request) {
	if s.guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	var body struct {
		AcceptURI string `json:"acceptUri"`
		AcceptCID string `json:"acceptCid"`
		Address   string `json:"address"`
		CreatedAt string `json:"createdAt"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxGuestBody+1))
	if err := dec.Decode(&body); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest"})
		return
	}
	reply := s.guest.OnNetwork(r.Context(), r.Header.Get("Authorization"), guestidentity.OnNetworkReport{
		AcceptURI: body.AcceptURI,
		AcceptCID: body.AcceptCID,
		Address:   body.Address,
		CreatedAt: body.CreatedAt,
	})
	writeReply(w, s.withGuestLogin(r.Context(), reply))
}

func (s *Service) withGuestLogin(ctx context.Context, reply guestidentity.Reply) guestidentity.Reply {
	if reply.Status != http.StatusOK || s.vms == nil || s.grants == nil {
		return reply
	}
	body := map[string]any{}
	if err := json.Unmarshal(reply.Body, &body); err != nil {
		return reply
	}
	name, _ := body["name"].(string)
	if name == "" {
		return reply
	}
	vm, err := s.vms.Get(ctx, name)
	if err != nil || vm == nil {
		return reply
	}
	ref, err := computevm.GuestLoginRef(*vm, s.guestLoginScopes(ctx, name))
	if err != nil {
		return reply
	}
	body["guestLogin"] = ref
	raw, err := json.Marshal(body)
	if err != nil {
		return reply
	}
	reply.Body = raw
	return reply
}

func (s *Service) guestLoginScopes(ctx context.Context, name string) []string {
	grantList, err := s.grants.List(ctx)
	if err != nil {
		return nil
	}
	scopes := []string{}
	for i := range grantList {
		if grantList[i].Spec.ComputeVMRef.Name != name {
			continue
		}
		scopes = append(scopes, grantList[i].Spec.Scopes...)
	}
	return scopes
}

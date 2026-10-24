package guestcontrolplane

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/guestcp"
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

func (s *Service) guest() guestcp.Plane {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.plane == nil {
		return nil
	}
	return s.plane
}

func writeReply(w http.ResponseWriter, reply guestcp.Reply) {
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
	guest := s.guest()
	if guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	mount := r.PathValue("mount")
	if mount != s.spec.GuestLoginMount {
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
	writeReply(w, guest.LoginStep(r.Context(), step, input))
}

func (s *Service) handleOIDCToken(w http.ResponseWriter, r *http.Request) {
	guest := s.guest()
	if guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	role := r.PathValue("role")
	if !guestidentity.ValidRole(role) {
		writeGuestError(w, http.StatusNotFound, "no such role")
		return
	}
	writeReply(w, guest.OIDCToken(r.Context(), role, r.Header.Get(guestidentity.VaultTokenHeader)))
}

func (s *Service) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	guest := s.guest()
	if guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	writeReply(w, guest.Discovery(r.Context()))
}

func (s *Service) handleKeys(w http.ResponseWriter, r *http.Request) {
	guest := s.guest()
	if guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	writeReply(w, guest.Keys(r.Context()))
}

func (s *Service) handleOnNetwork(w http.ResponseWriter, r *http.Request) {
	guest := s.guest()
	if guest == nil {
		writeGuestError(w, http.StatusServiceUnavailable, "the guest control plane is not ready")
		return
	}
	var body struct {
		AcceptURI string `json:"acceptUri"`
		AcceptCID string `json:"acceptCid"`
		Address   string `json:"address"`
		CreatedAt string `json:"createdAt"`
		Tag       string `json:"tag"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, MaxGuestBody+1))
	if err := dec.Decode(&body); err != nil {
		httpjson.Write(w, http.StatusBadRequest, map[string]any{"error": "InvalidRequest"})
		return
	}
	writeReply(w, guest.OnNetwork(r.Context(), r.Header.Get("Authorization"), guestcp.OnNetworkReport{
		AcceptURI: body.AcceptURI,
		AcceptCID: body.AcceptCID,
		Address:   body.Address,
		CreatedAt: body.CreatedAt,
		Tag:       body.Tag,
	}))
}

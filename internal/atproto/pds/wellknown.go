package pds

import (
	"net"
	"net/http"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpjson"
)

func (s *Server) handleWellKnownDID(w http.ResponseWriter, r *http.Request) {
	did := s.serviceDID
	if account := s.accountForHost(r); account != "" {
		did = account
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(did))
}

func (s *Server) accountForHost(r *http.Request) string {
	if s.store == nil {
		return ""
	}
	host := strings.TrimSpace(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || strings.HasPrefix(host, "did:") {
		return ""
	}
	acct, err := s.store.GetAccountByIdentifier(r.Context(), host)
	if err != nil || acct == nil || !acct.Active || !strings.EqualFold(acct.Handle, host) {
		return ""
	}
	return acct.DID.String()
}

func (s *Server) handleWellKnownDIDDocument(w http.ResponseWriter, r *http.Request) {
	doc := map[string]any{
		"@context": []string{
			"https://www.w3.org/ns/did/v1",
			"https://w3id.org/security/multikey/v1",
		},
		"id": s.serviceDID,
	}
	if len(s.cfg.AlsoKnownAs) > 0 {
		doc["alsoKnownAs"] = append([]string(nil), s.cfg.AlsoKnownAs...)
	}
	if s.cfg.OAuth != nil && s.cfg.OAuth.Signer.Key != nil {
		if pub, err := s.cfg.OAuth.Signer.Key.PublicKey(); err == nil {
			doc["verificationMethod"] = []map[string]any{{
				"id":                 s.serviceDID + "#atproto",
				"type":               "Multikey",
				"controller":         s.serviceDID,
				"publicKeyMultibase": pub.Multibase(),
			}}
		}
	}
	endpoint := s.cfg.PublicBaseURL
	if endpoint == "" {
		endpoint = s.publicURL(r)
	}
	doc["service"] = []map[string]any{{
		"id":              "#atproto_pds",
		"type":            "AtprotoPersonalDataServer",
		"serviceEndpoint": endpoint,
	}}
	httpjson.Write(w, http.StatusOK, doc)
}

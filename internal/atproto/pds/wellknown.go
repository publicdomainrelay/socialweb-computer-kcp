package pds

import (
	"net/http"
)

func (s *Server) handleWellKnownDID(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.serviceDID))
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
	writeJSON(w, http.StatusOK, doc)
}

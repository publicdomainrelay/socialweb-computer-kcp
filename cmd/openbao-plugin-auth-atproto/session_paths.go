package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const sessionDIDPattern = "(?P<did>did:[a-z0-9]+:[a-zA-Z0-9._:%-]+)"

var proofMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true}

func pathSession(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "session/" + sessionDIDPattern + "$",
		Fields: map[string]*framework.FieldSchema{
			"did": {Type: framework.TypeString, Description: "The DID of the session.", Required: true},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation:   &framework.PathOperation{Callback: b.sessionRead},
			logical.DeleteOperation: &framework.PathOperation{Callback: b.sessionDelete},
		},
		HelpSynopsis: "Reads a session without any secret, or signs it out: revokes its tokens at the authorization server and removes it.",
	}
}

func pathSessionProof(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "session/" + sessionDIDPattern + "/proof$",
		Fields: map[string]*framework.FieldSchema{
			"did":   {Type: framework.TypeString, Description: "The DID of the session.", Required: true},
			"htm":   {Type: framework.TypeString, Description: "HTTP method of the request the proof is for.", Required: true},
			"htu":   {Type: framework.TypeString, Description: "URL of the request the proof is for; it must be under the PDS origin of the session.", Required: true},
			"nonce": {Type: framework.TypeString, Description: "The latest DPoP-Nonce the PDS answered with, when the caller has one."},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.sessionProof},
		},
		HelpSynopsis: "Returns the DPoP-bound access token and one DPoP proof for one request. The key never leaves the mount.",
	}
}

func (b *backend) sessionRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	s, err := getSession(ctx, req.Storage, d.Get("did").(string))
	if err != nil || s == nil {
		return nil, err
	}
	return &logical.Response{Data: map[string]interface{}{
		"did":           s.DID,
		"handle":        s.Handle,
		"pds":           s.PDS,
		"issuer":        s.Issuer,
		"scope":         s.Scope,
		"expires_at":    s.ExpiresAt.Format(time.RFC3339),
		"status":        s.Status,
		"status_reason": s.StatusReason,
		"created_at":    s.CreatedAt.Format(time.RFC3339),
		"updated_at":    s.UpdatedAt.Format(time.RFC3339),
	}}, nil
}

func (b *backend) refreshSession(ctx context.Context, st logical.Storage, cfg *configEntry, s *sessionEntry) error {
	key, err := s.dpopKey()
	if err != nil {
		return err
	}
	f, err := newFetcher(cfg)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, cfg.LoginTimeout)
	defer cancel()
	oc := &oauthClient{f: f, issuer: s.Issuer, dpop: key, nonces: b.nonces.forDID(s.DID)}
	tr, err := oc.refresh(cctx, s.TokenEndpoint, s.RefreshToken)
	if err != nil {
		var oe *oauthError
		if errors.As(err, &oe) {
			s.Status, s.StatusReason = statusExpired, err.Error()
			if perr := putSession(ctx, st, s); perr != nil {
				return perr
			}
		}
		return err
	}
	if tr.Scope != "" && !strings.Contains(" "+tr.Scope+" ", " atproto ") {
		if s.RevocationEndpoint != "" {
			_ = oc.revoke(cctx, s.RevocationEndpoint, tr.RefreshToken, "refresh_token")
		}
		s.Status, s.StatusReason = statusExpired, "the refreshed grant does not include atproto"
		if perr := putSession(ctx, st, s); perr != nil {
			return perr
		}
		return errors.New("the refreshed grant does not include atproto; the session is rejected")
	}
	s.AccessToken, s.RefreshToken = tr.AccessToken, tr.RefreshToken
	s.ExpiresAt = time.Now().UTC().Add(time.Duration(tr.ExpiresIn) * time.Second)
	if tr.Scope != "" {
		s.Scope = tr.Scope
	}
	return putSession(ctx, st, s)
}

func (b *backend) sessionProof(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	did := d.Get("did").(string)
	htm := strings.ToUpper(strings.TrimSpace(d.Get("htm").(string)))
	htu := strings.TrimSpace(d.Get("htu").(string))
	nonce := strings.TrimSpace(d.Get("nonce").(string))
	if !proofMethods[htm] {
		return logical.ErrorResponse(fmt.Sprintf("htm %q is not an HTTP method a proof is made for", htm)), nil
	}
	origin, err := originOf(htu)
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf("htu %q: %v", htu, err)), nil
	}
	if u, _ := url.Parse(htu); u.User != nil {
		return logical.ErrorResponse("htu must not carry userinfo"), nil
	}
	lock := b.sessions.forDID(did)
	lock.Lock()
	defer lock.Unlock()
	s, err := getSession(ctx, req.Storage, did)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, logical.CodedError(http.StatusNotFound, "no session for "+did)
	}
	if strings.ToLower(origin) != strings.ToLower(s.PDS) {
		return logical.ErrorResponse(fmt.Sprintf("htu origin %s is not the PDS origin %s of the session; a proof is made only for requests to the PDS", origin, s.PDS)), nil
	}
	if s.Status != statusActive {
		return nil, logical.CodedError(http.StatusConflict, fmt.Sprintf("the session of %s is %s: %s", did, s.Status, s.StatusReason))
	}
	cfg, bad, err := requireConfig(ctx, req.Storage)
	if bad != nil || err != nil {
		return bad, err
	}
	if time.Until(s.ExpiresAt) < cfg.RefreshLeeway {
		if err := b.refreshSession(ctx, req.Storage, cfg, s); err != nil {
			return nil, logical.CodedError(http.StatusBadGateway, "refresh failed: "+err.Error())
		}
	}
	nonces := b.nonces.forDID(did)
	if nonce != "" {
		nonces.set(origin, nonce)
	}
	key, err := s.dpopKey()
	if err != nil {
		return nil, err
	}
	proof, err := dpopProof(key, htm, htu, nonces.get(origin), s.AccessToken)
	if err != nil {
		return nil, err
	}
	return &logical.Response{Data: map[string]interface{}{
		"access_token": s.AccessToken,
		"token_type":   "DPoP",
		"dpop_proof":   proof,
		"expires_at":   s.ExpiresAt.Format(time.RFC3339),
	}}, nil
}

func (b *backend) sessionDelete(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	did := d.Get("did").(string)
	lock := b.sessions.forDID(did)
	lock.Lock()
	defer lock.Unlock()
	s, err := getSession(ctx, req.Storage, did)
	if err != nil || s == nil {
		return nil, err
	}
	data := map[string]interface{}{"revoked": false}
	if s.RevocationEndpoint == "" {
		data["revoke_error"] = "the authorization server publishes no revocation endpoint"
	} else if cfg, bad, cerr := requireConfig(ctx, req.Storage); bad != nil || cerr != nil {
		data["revoke_error"] = "the mount has no config"
	} else if key, kerr := s.dpopKey(); kerr != nil {
		data["revoke_error"] = kerr.Error()
	} else if f, ferr := newFetcher(cfg); ferr != nil {
		data["revoke_error"] = ferr.Error()
	} else {
		cctx, cancel := context.WithTimeout(ctx, cfg.LoginTimeout)
		oc := &oauthClient{f: f, issuer: s.Issuer, dpop: key, nonces: b.nonces.forDID(did)}
		if rerr := oc.revoke(cctx, s.RevocationEndpoint, s.RefreshToken, "refresh_token"); rerr != nil {
			data["revoke_error"] = rerr.Error()
		} else {
			data["revoked"] = true
		}
		cancel()
	}
	if err := req.Storage.Delete(ctx, sessionPrefix+did); err != nil {
		return nil, err
	}
	return &logical.Response{Data: data}, nil
}

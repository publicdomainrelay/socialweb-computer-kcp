package guestidentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	RBACCollection = "com.fedproxy.rbac"

	maximumRBACPages = 100

	maximumRBACRecords = 5000
)

type PDSResolver interface {
	ResolvePDS(ctx context.Context, did string) (string, error)
}

type PDSResolverFunc func(context.Context, string) (string, error)

func (f PDSResolverFunc) ResolvePDS(ctx context.Context, did string) (string, error) {
	return f(ctx, did)
}

type DirectoryPDS struct {
	Directory identity.Directory
}

func (d DirectoryPDS) ResolvePDS(ctx context.Context, did string) (string, error) {
	if d.Directory == nil {
		return "", errors.New("guestidentity: the PDS resolver has no identity directory")
	}
	parsed, err := syntax.ParseDID(did)
	if err != nil {
		return "", fmt.Errorf("guestidentity: %q is not a DID: %w", did, err)
	}
	ident, err := d.Directory.LookupDID(ctx, parsed)
	if err != nil {
		return "", fmt.Errorf("guestidentity: resolving the PDS of %s: %w", did, err)
	}
	if ident == nil {
		return "", fmt.Errorf("guestidentity: %s resolves to no identity", did)
	}
	for _, service := range ident.Services {
		if service.Type == "AtprotoPersonalDataServer" {
			return service.URL, nil
		}
	}
	return "", fmt.Errorf("guestidentity: the DID document of %s names no PDS", did)
}

type HTTPRBAC struct {
	Resolver PDSResolver

	Client *http.Client

	Log *slog.Logger
}

func (h *HTTPRBAC) Records(ctx context.Context, requesterDID string) ([]RBACRecord, error) {
	if h == nil || h.Resolver == nil {
		return nil, errors.New("guestidentity: the RBAC reader has no PDS resolver")
	}
	pds, err := h.Resolver.ResolvePDS(ctx, requesterDID)
	if err != nil {
		return nil, err
	}
	base, err := pdsBase(pds)
	if err != nil {
		return nil, err
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	log := h.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	out := []RBACRecord{}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < maximumRBACPages; page++ {
		query := url.Values{"repo": {requesterDID}, "collection": {RBACCollection}, "limit": {"100"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/xrpc/com.atproto.repo.listRecords?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("guestidentity: listing the RBAC records of %s: %w", requesterDID, err)
		}
		status := resp.StatusCode
		if status != http.StatusOK {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, fmt.Errorf("guestidentity: listing the RBAC records of %s returned HTTP %d", requesterDID, status)
		}
		var body struct {
			Cursor  string `json:"cursor"`
			Records []struct {
				Value json.RawMessage `json:"value"`
			} `json:"records"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("guestidentity: the RBAC records of %s are not JSON: %w", requesterDID, err)
		}
		for _, record := range body.Records {
			var rb RBACRecord
			if err := json.Unmarshal(record.Value, &rb); err != nil {
				log.Warn("a requester RBAC record is not one and is skipped", "requester", requesterDID, "err", err.Error())
				continue
			}
			out = append(out, rb)
			if len(out) >= maximumRBACRecords {
				return out, nil
			}
		}
		if body.Cursor == "" {
			return out, nil
		}
		if seen[body.Cursor] {
			return nil, fmt.Errorf("guestidentity: the RBAC records of %s repeat cursor %q", requesterDID, body.Cursor)
		}
		seen[body.Cursor] = true
		cursor = body.Cursor
	}
	return nil, fmt.Errorf("guestidentity: the RBAC records of %s run past %d pages", requesterDID, maximumRBACPages)
}

func pdsBase(pds string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(pds))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("guestidentity: PDS endpoint %q is not a URL", pds)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("guestidentity: PDS endpoint %q is not http or https", pds)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("guestidentity: PDS endpoint %q carries a user, query or fragment", pds)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

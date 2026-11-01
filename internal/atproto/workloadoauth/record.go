package workloadoauth

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

type Client struct {
	ID string

	RedirectURI string

	Scopes []string
}

type Role struct {
	Name string

	Account string

	Service string

	Issuer string

	Subject string

	Clients []Client
}

func (r Role) Audience() string { return oauth.WorkloadAudience(r.Account, r.Name) }

func (r Role) Check() error {
	if _, err := syntax.ParseRecordKey(r.Name); err != nil {
		return fmt.Errorf("workloadoauth: role %q is not a record key: %w", r.Name, err)
	}
	if _, err := syntax.ParseDID(r.Account); err != nil {
		return fmt.Errorf("workloadoauth: account %q is not a DID: %w", r.Account, err)
	}
	if _, err := syntax.ParseDID(r.Service); err != nil {
		return fmt.Errorf("workloadoauth: service %q is not a DID: %w", r.Service, err)
	}
	if r.Issuer == "" || r.Subject == "" {
		return errors.New("workloadoauth: the role needs the workload's issuer and subject")
	}
	if len(r.Clients) == 0 {
		return errors.New("workloadoauth: the role names no OAuth client")
	}
	for _, c := range r.Clients {
		if err := c.Check(); err != nil {
			return err
		}
	}
	return nil
}

func (c Client) idSchema() map[string]any {
	if !strings.Contains(c.ID, "*") {
		return map[string]any{"const": c.ID}
	}
	parts := strings.Split(c.ID, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	return map[string]any{"type": "string", "pattern": "^" + strings.Join(parts, "[^?#*]*") + "$"}
}

func (c Client) Check() error {
	prefix, _, _ := strings.Cut(c.ID, "*")
	if strings.Contains(c.ID, "*") && strings.Count(strings.TrimPrefix(prefix, "https://"), "/") == 0 {
		return fmt.Errorf("workloadoauth: client %q must name its host and a path before any *", c.ID)
	}
	u, err := url.Parse(prefix)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && u.Hostname() == "localhost")) {
		return fmt.Errorf("workloadoauth: client %q is not an https client_id", c.ID)
	}
	if len(c.Scopes) == 0 {
		return fmt.Errorf("workloadoauth: client %s names no scope", c.ID)
	}
	for _, scope := range c.Scopes {
		if !oauth.ValidScope(scope) {
			return fmt.Errorf("workloadoauth: %q is not an atproto scope", scope)
		}
		if strings.HasPrefix(scope, "transition:") {
			return fmt.Errorf("workloadoauth: %s is a full-access scope a workload may not approve", scope)
		}
		if strings.HasPrefix(scope, "repo:"+workloadauth.RBACCollection) || strings.HasPrefix(scope, "repo:*") {
			return fmt.Errorf("workloadoauth: %s would let the app grant further workloads", scope)
		}
	}
	return nil
}

func Record(role Role, now time.Time) (map[string]any, error) {
	if err := role.Check(); err != nil {
		return nil, err
	}
	schemas := map[string]any{
		oauth.PathExchangeToken: map[string]any{
			"type":       "object",
			"required":   []any{"capability"},
			"properties": map[string]any{"capability": map[string]any{"enum": []any{workloadauth.CapabilityCreate}}},
		},
	}
	policies := map[string]any{}
	names := []any{}
	for i, client := range role.Clients {
		name := role.Name
		if len(role.Clients) > 1 {
			name = fmt.Sprintf("%s-%d", role.Name, i)
		}
		scopes := []any{}
		for _, s := range client.Scopes {
			scopes = append(scopes, s)
		}
		redirect := map[string]any{"type": "string"}
		if client.RedirectURI != "" {
			redirect = map[string]any{"const": client.RedirectURI}
		}
		body := map[string]any{
			"type":                 "object",
			"required":             []any{"client_id", "scope"},
			"additionalProperties": false,
			"properties": map[string]any{
				"client_id":    client.idSchema(),
				"redirect_uri": redirect,
				"scope":        map[string]any{"type": "array", "items": map[string]any{"enum": scopes}},
			},
		}
		clientSchemas := map[string]any{
			oauth.PathApproveAuthorization: map[string]any{
				"type":     "object",
				"required": []any{"capability", "body"},
				"properties": map[string]any{
					"capability": map[string]any{"enum": []any{workloadauth.CapabilityCreate}},
					"body":       body,
				},
			},
		}
		if i == 0 {
			clientSchemas[oauth.PathExchangeToken] = schemas[oauth.PathExchangeToken]
		}
		policies[name] = map[string]any{"meta": map[string]any{"policy": name}, "schemas": clientSchemas}
		names = append(names, name)
	}
	return map[string]any{
		"$type":    workloadauth.RBACCollection,
		"protects": map[string]any{role.Name: map[string]any{"service": role.Service, "scope": workloadauth.ProtectsScope}},
		"roles": map[string]any{role.Name: map[string]any{
			"role_name": role.Name,
			"definition": map[string]any{
				"iss":      role.Issuer,
				"aud":      role.Audience(),
				"sub":      role.Subject,
				"policies": names,
			},
		}},
		"policies":  policies,
		"createdAt": now.UTC().Format(time.RFC3339),
	}, nil
}

func ParseClient(raw string) (Client, error) {
	id, scopes, _ := strings.Cut(strings.TrimSpace(raw), "=")
	c := Client{ID: strings.TrimSpace(id)}
	for _, s := range strings.Split(scopes, ",") {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(c.Scopes, s) {
			c.Scopes = append(c.Scopes, s)
		}
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{oauth.ScopeAtproto}
	}
	return c, c.Check()
}

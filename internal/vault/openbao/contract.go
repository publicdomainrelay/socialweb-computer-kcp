package openbao

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"
)

var ErrOutOfScope = errors.New("openbao: the path is outside the handle's scope")

type ClientFor func(ctx context.Context, mount string) (*Client, error)

type Handle struct {
	clients ClientFor

	mount string

	scope []string
}

var (
	_ vault.KV = (*Handle)(nil)

	_ vault.Leases = (*Handle)(nil)

	_ vault.PKI = (*Handle)(nil)
)

func NewHandle(clients ClientFor, defaultMount string, scope []string) (*Handle, error) {
	if clients == nil {
		return nil, errors.New("openbao: a handle needs a client source")
	}
	cleaned := make([]string, 0, len(scope))
	for _, s := range scope {
		if s = strings.Trim(strings.TrimSpace(s), "/"); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	if len(cleaned) == 0 {
		return nil, fmt.Errorf("%w: a handle needs a scope", ErrOutOfScope)
	}
	return &Handle{clients: clients, mount: strings.Trim(defaultMount, "/"), scope: cleaned}, nil
}

func (h *Handle) allows(target string) bool {
	target = strings.Trim(target, "/")
	for _, s := range h.scope {
		if s == "*" || s == target {
			return true
		}
		if prefix, ok := strings.CutSuffix(s, "*"); ok && strings.HasPrefix(target, prefix) {
			return true
		}
		if strings.HasPrefix(target, s+"/") {
			return true
		}
	}
	return false
}

func (h *Handle) kvTarget(ctx context.Context, mount, path string) (*Client, string, string, error) {
	mount = strings.Trim(strings.TrimSpace(mount), "/")
	if mount == "" {
		mount = h.mount
	}
	path = strings.Trim(strings.TrimSpace(path), "/")
	path = strings.TrimPrefix(strings.TrimPrefix(path, mount+"/data/"), mount+"/metadata/")
	if mount == "" || path == "" {
		return nil, "", "", errors.New("openbao: a KV call needs a mount and a path")
	}
	if !h.allows(mount + "/" + path) {
		return nil, "", "", fmt.Errorf("%w: %s/%s", ErrOutOfScope, mount, path)
	}
	client, err := h.clients(ctx, mount)
	if err != nil {
		return nil, "", "", err
	}
	return client, mount, path, nil
}

func (h *Handle) Read(ctx context.Context, mount, path string, version int) (map[string]any, error) {
	client, mount, path, err := h.kvTarget(ctx, mount, path)
	if err != nil {
		return nil, err
	}
	target := mount + "/data/" + path
	if version > 0 {
		target = fmt.Sprintf("%s?version=%d", target, version)
	}
	return client.GetKV(ctx, target)
}

func (h *Handle) Write(ctx context.Context, mount, path string, data map[string]any) error {
	client, mount, path, err := h.kvTarget(ctx, mount, path)
	if err != nil {
		return err
	}
	if data == nil {
		data = map[string]any{}
	}
	return client.PutKV(ctx, mount+"/data/"+path, data)
}

func (h *Handle) Delete(ctx context.Context, mount, path string) error {
	client, mount, path, err := h.kvTarget(ctx, mount, path)
	if err != nil {
		return err
	}
	return client.Delete(ctx, mount+"/metadata/"+path)
}

func (h *Handle) Acquire(ctx context.Context, req vault.LeaseRequest) (vault.Lease, error) {
	role := strings.Trim(strings.TrimSpace(req.Role), "/")
	if role == "" || !h.allows(role) {
		return vault.Lease{}, fmt.Errorf("%w: lease role %q", ErrOutOfScope, req.Role)
	}
	client, err := h.clients(ctx, "")
	if err != nil {
		return vault.Lease{}, err
	}
	body := map[string]any{}
	if req.TTL > 0 {
		body["ttl"] = req.TTL.String()
	}
	_, raw, err := client.authCall(ctx, "auth/token/create/"+role, body)
	if err != nil {
		return vault.Lease{}, err
	}
	return vault.Lease{ID: raw.accessor, Role: role, Renewable: raw.renewable, ExpiresAt: leaseUntil(raw.lease)}, nil
}

func (h *Handle) Renew(ctx context.Context, id string, increment time.Duration) (vault.Lease, error) {
	client, err := h.clients(ctx, "")
	if err != nil {
		return vault.Lease{}, err
	}
	body := map[string]any{"accessor": id}
	if increment > 0 {
		body["increment"] = increment.String()
	}
	_, raw, err := client.authCall(ctx, "auth/token/renew-accessor", body)
	if err != nil {
		return vault.Lease{}, err
	}
	return vault.Lease{ID: id, Renewable: raw.renewable, ExpiresAt: leaseUntil(raw.lease)}, nil
}

func (h *Handle) Revoke(ctx context.Context, id string) error {
	client, err := h.clients(ctx, "")
	if err != nil {
		return err
	}
	return client.Write(ctx, "auth/token/revoke-accessor", map[string]any{"accessor": id})
}

func (h *Handle) Issue(ctx context.Context, mount, role string, req vault.CertRequest) (vault.Cert, error) {
	mount, role = strings.Trim(mount, "/"), strings.Trim(role, "/")
	if mount == "" || role == "" || strings.TrimSpace(req.CommonName) == "" {
		return vault.Cert{}, errors.New("openbao: issuing a certificate needs a PKI mount, a role and a common name")
	}
	if !h.allows(mount+"/"+role) && !h.allows(role) {
		return vault.Cert{}, fmt.Errorf("%w: PKI role %s/%s", ErrOutOfScope, mount, role)
	}
	client, err := h.clients(ctx, "")
	if err != nil {
		return vault.Cert{}, err
	}
	return Backend{Client: client}.Issue(ctx, "", mount, role, req)
}

func (h *Handle) CAChain(ctx context.Context, mount string) (string, error) {
	client, err := h.clients(ctx, "")
	if err != nil {
		return "", err
	}
	return client.CAChainPEM(ctx, mount)
}

func (c *Client) authCall(ctx context.Context, path string, body map[string]any) (int, loginAuth, error) {
	status, raw, err := c.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return status, loginAuth{}, err
	}
	if status < 200 || status > 299 {
		return status, loginAuth{}, &ResponseError{Method: http.MethodPost, Path: path, Status: status, Body: strings.TrimSpace(string(raw))}
	}
	auth, err := decodeAuth(path, raw, false)
	return status, auth, err
}

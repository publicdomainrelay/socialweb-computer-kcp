package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/helper/tokenutil"
	"github.com/openbao/openbao/sdk/v2/logical"
	"golang.org/x/crypto/ssh"
)

const (
	rolePrefix          = "role/"
	defaultChallengeTTL = 60 * time.Second
	maxChallengeTTL     = 10 * time.Minute
	maxChallenges       = 16
)

type roleEntry struct {
	tokenutil.TokenParams

	AliasName      string               `json:"alias_name"`
	ChallengeTTL   time.Duration        `json:"challenge_ttl"`
	BindSecretHash string               `json:"bind_secret_hash,omitempty"`
	HostKey        string               `json:"host_key,omitempty"`
	BoundAt        time.Time            `json:"bound_at,omitempty"`
	Challenges     map[string]time.Time `json:"challenges,omitempty"`
	WIDSubject     string               `json:"wid_subject,omitempty"`
	WIDClaims      map[string]string    `json:"wid_claims,omitempty"`
}

func (r *roleEntry) hostKey() (ssh.PublicKey, error) {
	if r.HostKey == "" {
		return nil, nil
	}
	return parseEd25519AuthorizedKey(r.HostKey)
}

func (r *roleEntry) hostKeyFingerprint() string {
	key, err := r.hostKey()
	if err != nil || key == nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

func getRole(ctx context.Context, s logical.Storage, name string) (*roleEntry, error) {
	raw, err := s.Get(ctx, rolePrefix+name)
	if err != nil || raw == nil {
		return nil, err
	}
	var r roleEntry
	if err := raw.DecodeJSON(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func putRole(ctx context.Context, s logical.Storage, name string, r *roleEntry) error {
	entry, err := logical.StorageEntryJSON(rolePrefix+name, r)
	if err != nil {
		return err
	}
	return s.Put(ctx, entry)
}

func pathRoleList(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "role/?$",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ListOperation: &framework.PathOperation{Callback: b.roleList},
		},
		HelpSynopsis: "Lists the roles.",
	}
}

func pathRole(b *backend) *framework.Path {
	fields := map[string]*framework.FieldSchema{
		"name": {
			Type:        framework.TypeString,
			Description: "Name of the role.",
			Required:    true,
		},
		"bind_secret_hash": {
			Type:        framework.TypeString,
			Description: "Hex sha256 of the one-time bind secret. Required when the role is created; refused once the role is bound.",
		},
		"alias_name": {
			Type:        framework.TypeString,
			Description: "Name of the entity alias a login creates. Defaults to the role name.",
		},
		"challenge_ttl": {
			Type:        framework.TypeDurationSecond,
			Description: "Lifetime of a login challenge. Defaults to 60s, at most 10m.",
		},
		"wid_subject": {
			Type:        framework.TypeString,
			Description: "The sub of the workload identity tokens wid/token issues for this role.",
		},
		"wid_claims": {
			Type:        framework.TypeKVPairs,
			Description: "Extra string claims of the workload identity tokens; iss, sub, aud, exp, nbf, iat and jti are refused.",
		},
	}
	tokenutil.AddTokenFields(fields)
	return &framework.Path{
		Pattern:        "role/" + framework.GenericNameRegex("name"),
		Fields:         fields,
		ExistenceCheck: b.roleExists,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.CreateOperation: &framework.PathOperation{Callback: b.roleWrite},
			logical.UpdateOperation: &framework.PathOperation{Callback: b.roleWrite},
			logical.ReadOperation:   &framework.PathOperation{Callback: b.roleRead},
			logical.DeleteOperation: &framework.PathOperation{Callback: b.roleDelete},
		},
		HelpSynopsis: "Manages a role: its one-time bind secret hash, its token parameters and its bound host key.",
	}
}

func (b *backend) roleExists(ctx context.Context, req *logical.Request, d *framework.FieldData) (bool, error) {
	r, err := getRole(ctx, req.Storage, d.Get("name").(string))
	return r != nil, err
}

func (b *backend) roleList(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	names, err := req.Storage.List(ctx, rolePrefix)
	if err != nil {
		return nil, err
	}
	return logical.ListResponse(names), nil
}

func (b *backend) roleWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	name := d.Get("name").(string)
	b.lock.Lock()
	defer b.lock.Unlock()
	r, err := getRole(ctx, req.Storage, name)
	if err != nil {
		return nil, err
	}
	if r == nil {
		r = &roleEntry{AliasName: name, ChallengeTTL: defaultChallengeTTL}
	}
	if raw, ok := d.GetOk("bind_secret_hash"); ok {
		hash := strings.ToLower(strings.TrimSpace(raw.(string)))
		if decoded, err := hex.DecodeString(hash); err != nil || len(decoded) != 32 {
			return logical.ErrorResponse("bind_secret_hash must be 64 hex characters (sha256)"), nil
		}
		if r.HostKey != "" {
			return logical.ErrorResponse(fmt.Sprintf("role %q is already bound; delete it to bind again", name)), nil
		}
		r.BindSecretHash = hash
	}
	if r.HostKey == "" && r.BindSecretHash == "" {
		return logical.ErrorResponse("bind_secret_hash is required"), nil
	}
	if raw, ok := d.GetOk("alias_name"); ok {
		alias := strings.TrimSpace(raw.(string))
		if alias == "" {
			return logical.ErrorResponse("alias_name must not be empty"), nil
		}
		r.AliasName = alias
	}
	if raw, ok := d.GetOk("challenge_ttl"); ok {
		ttl := time.Duration(raw.(int)) * time.Second
		if ttl <= 0 || ttl > maxChallengeTTL {
			return logical.ErrorResponse(fmt.Sprintf("challenge_ttl must be between 1s and %s", maxChallengeTTL)), nil
		}
		r.ChallengeTTL = ttl
	}
	if raw, ok := d.GetOk("wid_subject"); ok {
		subject := strings.TrimSpace(raw.(string))
		if subject == "" || len(subject) > 512 {
			return logical.ErrorResponse("wid_subject must be 1 to 512 characters"), nil
		}
		r.WIDSubject = subject
	}
	if raw, ok := d.GetOk("wid_claims"); ok {
		claims := raw.(map[string]string)
		for k := range claims {
			if widReservedClaims[k] {
				return logical.ErrorResponse(fmt.Sprintf("wid_claims may not set %q", k)), nil
			}
		}
		r.WIDClaims = claims
	}
	if err := r.ParseTokenFields(req, d); err != nil {
		return logical.ErrorResponse(err.Error()), logical.ErrInvalidRequest
	}
	if err := putRole(ctx, req.Storage, name, r); err != nil {
		return nil, err
	}
	return nil, nil
}

func (b *backend) roleRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	r, err := getRole(ctx, req.Storage, d.Get("name").(string))
	if err != nil || r == nil {
		return nil, err
	}
	data := map[string]interface{}{
		"alias_name":           r.AliasName,
		"challenge_ttl":        int64(r.ChallengeTTL.Seconds()),
		"bind_secret_pending":  r.BindSecretHash != "",
		"bound":                r.HostKey != "",
		"host_key":             r.HostKey,
		"host_key_fingerprint": r.hostKeyFingerprint(),
		"wid_subject":          r.WIDSubject,
		"wid_claims":           r.WIDClaims,
	}
	if !r.BoundAt.IsZero() {
		data["bound_at"] = r.BoundAt.UTC().Format(time.RFC3339)
	}
	r.PopulateTokenData(data)
	return &logical.Response{Data: data}, nil
}

func (b *backend) roleDelete(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return nil, req.Storage.Delete(ctx, rolePrefix+d.Get("name").(string))
}

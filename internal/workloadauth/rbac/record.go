package rbac

import (
	"encoding/json"
	"fmt"
	"regexp"
)

var roleNameRE = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,512}$`)

type Record struct {
	Type string `json:"$type"`

	Protects map[string]Protects `json:"protects"`

	Roles map[string]Role `json:"roles"`

	Policies map[string]Policy `json:"policies"`

	CreatedAt string `json:"createdAt,omitempty"`
}

type Protects struct {
	Service string `json:"service"`

	Scope string `json:"scope"`
}

type Role struct {
	RoleName string `json:"role_name"`

	Definition Definition `json:"definition"`
}

type Definition struct {
	Iss string `json:"iss"`

	Aud string `json:"aud"`

	Sub string `json:"sub"`

	Policies []string `json:"policies"`
}

type Policy struct {
	Meta map[string]string `json:"meta,omitempty"`

	Schemas map[string]json.RawMessage `json:"schemas"`
}

func ParseRecord(raw []byte, rkey string) (Record, error) {
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("rbac: the record is not JSON: %w", err)
	}
	if err := rec.check(rkey); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (r Record) check(rkey string) error {
	if r.Type != "" && r.Type != "com.fedproxy.rbac" {
		return fmt.Errorf("rbac: the record is a %s", r.Type)
	}
	if !roleNameRE.MatchString(rkey) {
		return fmt.Errorf("rbac: the rkey %q is not a role name", rkey)
	}
	if len(r.Roles) != 1 {
		return fmt.Errorf("rbac: the record holds %d roles, not exactly one", len(r.Roles))
	}
	role, ok := r.Roles[rkey]
	if !ok {
		return fmt.Errorf("rbac: the record's role is not named %q, its rkey", rkey)
	}
	if role.RoleName != rkey {
		return fmt.Errorf("rbac: role_name %q is not the rkey %q", role.RoleName, rkey)
	}
	def := role.Definition
	if def.Iss == "" || def.Aud == "" || def.Sub == "" {
		return fmt.Errorf("rbac: the role %q needs iss, aud and sub", rkey)
	}
	if len(def.Policies) == 0 {
		return fmt.Errorf("rbac: the role %q names no policy", rkey)
	}
	for _, name := range def.Policies {
		policy, ok := r.Policies[name]
		if !ok {
			return fmt.Errorf("rbac: the role %q names policy %q, which the record does not hold", rkey, name)
		}
		for path, raw := range policy.Schemas {
			if _, err := parsePathSchema(raw); err != nil {
				return fmt.Errorf("rbac: policy %q path %q: %w", name, path, err)
			}
		}
	}
	return nil
}

func (r Record) Role() (string, Role) {
	for name, role := range r.Roles {
		return name, role
	}
	return "", Role{}
}

func (r Record) ProtectsFor(service, scope string) bool {
	for _, p := range r.Protects {
		if p.Service == service && p.Scope == scope {
			return true
		}
	}
	return false
}

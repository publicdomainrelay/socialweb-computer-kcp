package rbac

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

type Config struct {
	Service string

	Scope string
}

type Authorizer struct {
	service string

	scope string

	records workloadauth.RecordReader

	tokens workloadauth.TokenVerifier
}

var _ workloadauth.Authorizer = (*Authorizer)(nil)

func New(cfg Config, records workloadauth.RecordReader, tokens workloadauth.TokenVerifier) (*Authorizer, error) {
	if cfg.Service == "" {
		return nil, errors.New("rbac: the protected service is required")
	}
	if records == nil || tokens == nil {
		return nil, errors.New("rbac: a record reader and a token verifier are required")
	}
	scope := cfg.Scope
	if scope == "" {
		scope = workloadauth.ProtectsScope
	}
	return &Authorizer{service: cfg.Service, scope: scope, records: records, tokens: tokens}, nil
}

func (a *Authorizer) record(ctx context.Context, account, role string) (Record, error) {
	raw, err := a.records.Record(ctx, account, workloadauth.RBACCollection, role)
	if errors.Is(err, workloadauth.ErrRecordNotFound) {
		return Record{}, fmt.Errorf("%w: %s/%s", workloadauth.ErrNoGrant, account, role)
	}
	if err != nil {
		return Record{}, fmt.Errorf("rbac: reading %s/%s/%s: %w", account, workloadauth.RBACCollection, role, err)
	}
	rec, err := ParseRecord(raw, role)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %v", workloadauth.ErrNoGrant, err)
	}
	if !rec.ProtectsFor(a.service, a.scope) {
		return Record{}, fmt.Errorf("%w: the record does not protect %s scope %s", workloadauth.ErrNoGrant, a.service, a.scope)
	}
	return rec, nil
}

func (a *Authorizer) Admit(ctx context.Context, account, role, subjectToken string, action workloadauth.Action) (workloadauth.Grant, error) {
	rec, err := a.record(ctx, account, role)
	if err != nil {
		return workloadauth.Grant{}, err
	}
	_, r := rec.Role()
	def := r.Definition
	claims, err := a.tokens.Verify(ctx, subjectToken, def.Iss, def.Aud)
	if err != nil {
		return workloadauth.Grant{}, fmt.Errorf("%w: the workload token: %v", workloadauth.ErrDenied, err)
	}
	if claims.Issuer != def.Iss || claims.Subject != def.Sub || !slices.Contains(claims.Audience, def.Aud) {
		return workloadauth.Grant{}, fmt.Errorf("%w: the workload token is not the role's issuer, subject and audience", workloadauth.ErrDenied)
	}
	if err := rec.Allows(action); err != nil {
		return workloadauth.Grant{}, err
	}
	return workloadauth.Grant{Account: account, Role: role, Issuer: def.Iss, Subject: def.Sub, Audience: def.Aud,
		NotAfter: claims.Expiry, TokenID: claims.ID}, nil
}

func (a *Authorizer) Check(ctx context.Context, grant workloadauth.Grant, action workloadauth.Action) error {
	rec, err := a.record(ctx, grant.Account, grant.Role)
	if err != nil {
		return err
	}
	_, r := rec.Role()
	def := r.Definition
	if def.Iss != grant.Issuer || def.Sub != grant.Subject || def.Aud != grant.Audience {
		return fmt.Errorf("%w: the record no longer names this workload", workloadauth.ErrDenied)
	}
	return rec.Allows(action)
}

package workloadauth

import (
	"context"
	"errors"
	"time"
)

const (
	RBACCollection = "com.fedproxy.rbac"

	ProtectsScope = "com.fedproxy.temp.oauth"

	CapabilityRead = "read"

	CapabilityCreate = "create"

	CapabilityUpdate = "update"

	CapabilityDelete = "delete"
)

var (
	ErrNoGrant = errors.New("workloadauth: no com.fedproxy.rbac record grants this role")

	ErrDenied = errors.New("workloadauth: the record does not allow this")

	ErrRecordNotFound = errors.New("workloadauth: record not found")
)

type Action struct {
	Path string

	Method string

	Body any
}

type Grant struct {
	Account string

	Role string

	Issuer string

	Subject string

	Audience string

	NotAfter time.Time

	TokenID string
}

type Claims struct {
	Issuer string

	Subject string

	Audience []string

	Expiry time.Time

	NotBefore time.Time

	IssuedAt time.Time

	ID string
}

type RecordReader interface {
	Record(ctx context.Context, account, collection, rkey string) ([]byte, error)
}

type TokenVerifier interface {
	Verify(ctx context.Context, token, issuer, audience string) (Claims, error)
}

type Authorizer interface {
	Admit(ctx context.Context, account, role, subjectToken string, action Action) (Grant, error)

	Check(ctx context.Context, grant Grant, action Action) error
}

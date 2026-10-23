package gateway

import (
	"context"
	"encoding/json"
)

type WebUser struct {
	DID    string
	Handle string
	PDS    string
}

type WebCaller struct {
	WebUser
	Token string
}

type SignInStart struct {
	AuthorizationURL string
	State            string
}

type SignInCompleteParams struct {
	State string
	Code  string
	ISS   string
}

type SignInGrant struct {
	WebUser
	Token        string
	LeaseSeconds int64
}

type SignOutResult struct {
	Revoked     bool
	RevokeError string
}

type RecordRef struct {
	URI string
	CID string
}

type Record struct {
	URI   string
	CID   string
	Value map[string]any
}

type Write struct {
	Action     string
	Collection string
	Rkey       string
	Record     map[string]any
}

type WebAccountAgent interface {
	GetServiceAuth(ctx context.Context, aud, lxm string) (string, error)
	ListRecords(ctx context.Context, did, collection string, limit int) ([]Record, error)
	CreateRecord(ctx context.Context, did, collection, rkey string, record map[string]any) (RecordRef, error)
	ApplyWrites(ctx context.Context, did string, writes []Write) error
}

type WebSignIn interface {
	ClientMetadata(ctx context.Context) (json.RawMessage, error)
	Start(ctx context.Context, identifier, redirectURI string) (SignInStart, error)
	Complete(ctx context.Context, params SignInCompleteParams) (SignInGrant, error)
	Identify(ctx context.Context, token string) (*WebUser, error)
	SignOut(ctx context.Context, caller *WebCaller) (SignOutResult, error)
	AgentFor(caller *WebCaller) WebAccountAgent
}

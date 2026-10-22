package statusstate

import (
	"context"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
)

type PDSResolver interface {
	ResolvePDS(ctx context.Context, ref string) (pdsclient.PDS, error)
}

type PDSResolverFunc func(ctx context.Context, ref string) (pdsclient.PDS, error)

func (f PDSResolverFunc) ResolvePDS(ctx context.Context, ref string) (pdsclient.PDS, error) {
	return f(ctx, ref)
}

type RepoObservation struct {
	Head string

	Rev string

	Seq int64

	KeyFingerprint string
}

type RepoDetails struct {
	Seq int64

	KeyFingerprint string
}

type RepoDetailsSource interface {
	RepoDetails(ctx context.Context, did syntax.DID) (RepoDetails, error)
}

type AccountObservation struct {
	Active bool

	HasPassword bool

	AppPasswordCount int32

	SessionCount int32
}

type AccountSource interface {
	AccountStatus(ctx context.Context, ref, did string) (AccountObservation, error)
}

type AccountSourceFunc func(ctx context.Context, ref, did string) (AccountObservation, error)

func (f AccountSourceFunc) AccountStatus(ctx context.Context, ref, did string) (AccountObservation, error) {
	return f(ctx, ref, did)
}

type SessionObservation struct {
	Active bool

	ExpiresAt string

	Revoked bool
}

type SessionSource interface {
	SessionStatus(ctx context.Context, ref, did, sessionID string) (SessionObservation, error)
}

type SessionSourceFunc func(ctx context.Context, ref, did, sessionID string) (SessionObservation, error)

func (f SessionSourceFunc) SessionStatus(ctx context.Context, ref, did, sessionID string) (SessionObservation, error) {
	return f(ctx, ref, did, sessionID)
}

type PLCClient interface {
	AuditLog(ctx context.Context, did string) ([]plc.LogEntry, error)
}

type RelayObservation struct {
	Phase v1alpha1.RelayStatusPhase

	Cursor int64

	LastEventAt string

	Events int64
}

type RelaySource interface {
	RelayStatus(ctx context.Context, ref string) (RelayObservation, error)
}

type RelaySourceFunc func(ctx context.Context, ref string) (RelayObservation, error)

func (f RelaySourceFunc) RelayStatus(ctx context.Context, ref string) (RelayObservation, error) {
	return f(ctx, ref)
}

type VaultCredentialObservation struct {
	Exists bool

	ExpiresAt string

	RotationDue bool
}

type VaultCertificateObservation struct {
	Exists bool

	SerialNumber string

	Fingerprint string

	ExpiresAt string

	RotationDue bool
}

type VaultSource interface {
	CredentialMetadata(ctx context.Context, server, mount, path string) (VaultCredentialObservation, error)

	CertificateMetadata(ctx context.Context, server, mount, role, commonName string) (VaultCertificateObservation, error)
}

const DefaultRotationWindow = 30 * 24 * time.Hour

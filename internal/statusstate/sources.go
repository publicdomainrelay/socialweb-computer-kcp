package statusstate

import (
	"context"
	"fmt"
	"strings"
	"time"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
)

type AccountObservation struct {
	Active bool

	HasPassword bool

	AppPasswordCount int32

	SessionCount int32
}

type AccountSource interface {
	AccountStatus(ctx context.Context, from, pds commonv1.ObjectRef, did string) (AccountObservation, error)
}

type SessionObservation struct {
	Active bool

	ExpiresAt string

	Revoked bool
}

type SessionSource interface {
	SessionStatus(ctx context.Context, from, pds commonv1.ObjectRef, did, sessionID string) (SessionObservation, error)
}

type BusAccounts struct {
	Bus *bus.Bus
}

func (s BusAccounts) AccountStatus(ctx context.Context, from, to commonv1.ObjectRef, did string) (AccountObservation, error) {
	accounts, err := bus.Resolve[pds.Accounts](ctx, s.Bus, from, to, bus.Capability{Name: pds.CapabilityAccount, Scope: []string{did}})
	if err != nil {
		return AccountObservation{}, err
	}
	account, err := accounts.Account(ctx, did)
	if err != nil {
		return AccountObservation{}, err
	}
	return AccountObservation{Active: account.Active}, nil
}

type BusSessions struct {
	Bus *bus.Bus
}

func (s BusSessions) SessionStatus(ctx context.Context, from, to commonv1.ObjectRef, did, _ string) (SessionObservation, error) {
	sessions, err := bus.Resolve[pds.Sessions](ctx, s.Bus, from, to, bus.Capability{Name: pds.CapabilitySession, Scope: []string{did}})
	if err != nil {
		return SessionObservation{}, err
	}
	session, err := sessions.Session(ctx, did)
	if err != nil {
		return SessionObservation{}, err
	}
	if session.DID != "" && session.DID != did {
		return SessionObservation{}, fmt.Errorf("statusstate: the session is for %s, not %s", session.DID, did)
	}
	return SessionObservation{Active: session.Active}, nil
}

func pdsTarget(workspace string, ref commonv1.ObjectRef) commonv1.ObjectRef {
	out := ref
	out.Workspace = ref.InWorkspace(workspace)
	if out.Group == "" {
		out.Group = v1alpha1.GroupName
		if strings.EqualFold(strings.TrimSpace(ref.Kind), "RemotePDS") {
			out.Group = pdsv1alpha1.GroupName
		}
	}
	return out
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

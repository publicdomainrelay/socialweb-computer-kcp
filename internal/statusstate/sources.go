package statusstate

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func RepoBindingFor(workspace, ref, did string) pdsclient.RepoBinding {
	binding := pdsclient.RepoBinding{Workspace: workspace, DID: strings.TrimSpace(did)}
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
	case strings.Contains(ref, "://"):
		binding.Kind = "url"
		binding.Endpoint = ref
	default:
		if kind, name, ok := strings.Cut(ref, "/"); ok {
			binding.Kind = strings.TrimSpace(kind)
			binding.Name = strings.TrimSpace(name)
		} else {
			binding.Kind = "pds"
			binding.Name = ref
		}
	}
	return binding
}

type RepoAccountSource struct {
	Resolver *pdsclient.RepoResolver

	Workspace string
}

func (s RepoAccountSource) AccountStatus(ctx context.Context, ref, did string) (AccountObservation, error) {
	if s.Resolver == nil {
		return AccountObservation{}, errors.New("statusstate: no repo resolver is configured")
	}
	binding := RepoBindingFor(s.Workspace, ref, did)
	resolved, err := s.Resolver.Resolve(ctx, binding)
	if err != nil {
		return AccountObservation{}, err
	}
	if resolved.Tokens != nil {
		session, err := resolved.PDS.GetSession(ctx)
		if err == nil {
			if session.DID.String() != "" && session.DID.String() != did {
				return AccountObservation{}, fmt.Errorf("statusstate: the session is for %s, not %s", session.DID.String(), did)
			}
			return AccountObservation{Active: session.Active}, nil
		}
		if !errors.Is(err, pdsclient.ErrAuthenticationRequired) {
			return AccountObservation{}, err
		}
	}
	if _, _, err := resolved.PDS.GetLatestCommit(ctx, mustDID(did)); err != nil {
		return AccountObservation{}, err
	}
	return AccountObservation{Active: true}, nil
}

type RepoSessionSource struct {
	Resolver *pdsclient.RepoResolver

	Workspace string
}

func (s RepoSessionSource) SessionStatus(ctx context.Context, ref, did, sessionID string) (SessionObservation, error) {
	if s.Resolver == nil {
		return SessionObservation{}, errors.New("statusstate: no repo resolver is configured")
	}
	binding := RepoBindingFor(s.Workspace, ref, did)
	binding.SessionRef = strings.TrimSpace(sessionID)
	resolved, err := s.Resolver.Resolve(ctx, binding)
	if err != nil {
		return SessionObservation{}, err
	}
	session, err := resolved.PDS.GetSession(ctx)
	if err != nil {
		return SessionObservation{}, err
	}
	if session.DID.String() != "" && session.DID.String() != did {
		return SessionObservation{}, fmt.Errorf("statusstate: the session is for %s, not %s", session.DID.String(), did)
	}
	return SessionObservation{Active: session.Active}, nil
}

func mustDID(raw string) syntax.DID {
	did, err := syntax.ParseDID(strings.TrimSpace(raw))
	if err != nil {
		return syntax.DID(raw)
	}
	return did
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

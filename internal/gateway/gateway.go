package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/revocation"
)

const MinClientCertTTL = 60 * time.Second

const (
	DefaultClientCertTTL = 8 * time.Hour
	MaxClientCertTTL     = 24 * time.Hour
)

var ErrIssuerUnavailable = errors.New("gateway: the certificate issuer cannot issue now")

type Caller string

type XRPCError struct {
	Name    string
	Message string
	Status  int
}

func (e *XRPCError) Error() string { return e.Name + ": " + e.Message }

func (e *XRPCError) HTTPStatus() int { return e.Status }

var (
	ErrInvalidRequest  = &XRPCError{"InvalidRequest", "the request is malformed", 400}
	ErrUnauthorized    = &XRPCError{"Unauthorized", "the caller is not authenticated", 401}
	ErrForbidden       = &XRPCError{"Forbidden", "the caller may not act for that identity", 403}
	ErrIdentityRevoked = &XRPCError{
		"IdentityRevoked", "this identity has been revoked", 403,
	}
	ErrClusterNotFound = &XRPCError{"ClusterNotFound", "no such cluster", 404}

	ErrClusterInitializing = &XRPCError{
		"ClusterInitializing", "the control plane is coming up", 429,
	}

	ErrClusterDraining = &XRPCError{"ClusterDraining", "the cluster is draining", 409}

	ErrClusterUnavailable = &XRPCError{
		"ClusterUnavailable", "the identity store could not be read", 503,
	}

	ErrNoDenyWriter = &XRPCError{
		"ClusterUnavailable",
		"this gateway was built without a revocation writer, so no deny can be written",
		503,
	}
)

func errForDeny(err error) *XRPCError {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, revocation.ErrRevoked):
		return ErrIdentityRevoked
	case errors.Is(err, revocation.ErrDenyRecordMissing):
		return &XRPCError{
			"IdentityRevoked",
			"the account's identity record is missing, which is not a state a user can produce",
			403,
		}
	case errors.Is(err, revocation.ErrNoDID):
		return ErrUnauthorized
	case errors.Is(err, revocation.ErrDenyUnreadable):
		return ErrClusterUnavailable
	default:
		return unprepared(err)
	}
}

const maxCause = 300

func unprepared(cause error) *XRPCError {
	return &XRPCError{
		"ClusterUnavailable",
		"the cluster could not be prepared: " + oneLine(cause.Error()),
		503,
	}
}

func oneLine(msg string) string {
	line := strings.Join(strings.Fields(msg), " ")
	if len(line) > maxCause {
		line = line[:maxCause] + "..."
	}
	return line
}

type ClusterView struct {
	Name     string
	Phase    string
	URL      string
	Instance string
	Running  bool

	TeardownBlocked bool

	NodeCount int
}

type Store interface {
	Ensure(ctx context.Context, caller Caller, name string) error

	Kubeconfig(ctx context.Context, caller Caller, name string, ttl time.Duration, noExpiry bool) (string, error)

	Get(ctx context.Context, caller Caller, name string) (ClusterView, bool, error)
	List(ctx context.Context, caller Caller) ([]ClusterView, error)
	SetRunning(ctx context.Context, caller Caller, name string, running bool) (ClusterView, error)
	Delete(ctx context.Context, caller Caller, name string) (ClusterView, error)
}

type Revoker interface {
	WriteDeny(ctx context.Context, did string) error

	FanOut(ctx context.Context, did string) ([]revocation.ClusterView, error)
}

type CertRevoker interface {
	Revoke(ctx context.Context, did, cluster string) (int, error)
}

type Server struct {
	Deny  revocation.Deny
	Store Store

	Revoker Revoker

	Certs CertRevoker

	Feed Feed

	Tenant TenantAPI
}

type NameRequest struct {
	Name string
}

type KubeconfigRequest struct {
	Name string

	TTLSeconds *int64

	NoExpiry bool
}

type GetKubeconfigResponse struct {
	Kubeconfig string
	Instance   string
}

func (s *Server) GetKubeconfig(ctx context.Context, caller Caller, req KubeconfigRequest) (*GetKubeconfigResponse, error) {
	if req.NoExpiry && req.TTLSeconds != nil {
		return nil, ErrInvalidRequest
	}
	ttl, err := clientCertTTL(req.TTLSeconds)
	if err != nil {
		return nil, err
	}
	if req.NoExpiry {
		ttl = 0
	}
	if err := s.create(ctx, "getKubeconfig", caller, req.Name); err != nil {
		return nil, err
	}

	view, ok, err := s.Store.Get(ctx, caller, req.Name)
	if err != nil {
		return nil, fmt.Errorf("getKubeconfig: %w", err)
	}
	if !ok {
		return nil, ErrClusterNotFound
	}
	if !view.Running {
		return nil, ErrClusterNotRunning
	}
	kubeconfig, err := s.Store.Kubeconfig(ctx, caller, req.Name, ttl, req.NoExpiry)
	if err != nil {
		if errors.Is(err, ErrIssuerUnavailable) {
			slog.Warn("getKubeconfig: the certificate issuer is not ready", "caller", string(caller), "name", req.Name, "err", err)
			return nil, ErrClusterInitializing
		}
		return nil, fmt.Errorf("getKubeconfig: %w", err)
	}

	return &GetKubeconfigResponse{Kubeconfig: kubeconfig, Instance: view.Instance}, nil
}

func (s *Server) CreateCluster(ctx context.Context, caller Caller, req NameRequest) (ClusterView, error) {
	if err := s.create(ctx, "createCluster", caller, req.Name); err != nil {
		return ClusterView{}, err
	}
	view, err := s.Store.SetRunning(context.WithoutCancel(ctx), caller, req.Name, true)
	if err != nil {
		return ClusterView{}, err
	}
	if view.Phase == "Draining" {
		return ClusterView{}, ErrClusterDraining
	}
	return view, nil
}

func clientCertTTL(seconds *int64) (time.Duration, error) {
	if seconds == nil {
		return DefaultClientCertTTL, nil
	}
	if *seconds < int64(MinClientCertTTL/time.Second) || *seconds > int64(MaxClientCertTTL/time.Second) {
		return 0, ErrInvalidRequest
	}
	return time.Duration(*seconds) * time.Second, nil
}

func (s *Server) create(ctx context.Context, verb string, caller Caller, name string) error {
	if caller == "" {
		return ErrUnauthorized
	}
	if name == "" {
		return ErrInvalidRequest
	}

	permit, err := revocation.CreateOnRead(ctx, s.Deny, string(caller), func(ctx context.Context) error {
		return s.Store.Ensure(ctx, caller, name)
	})
	if err != nil {
		slog.Error("gateway: the create-on-read path failed",
			"verb", verb, "caller", string(caller), "name", name, "err", err)
		if x := errForDeny(err); x != nil {
			return x
		}
		return fmt.Errorf("%s: %w", verb, err)
	}

	if permit.Admitted() != string(caller) {
		return fmt.Errorf("%s: permit is for %q, caller is %q", verb, permit.Admitted(), caller)
	}
	return nil
}

func (s *Server) ControlPlaneUp(ctx context.Context, caller Caller, req NameRequest) (ClusterView, error) {
	view, err := s.lookup(ctx, caller, req)
	if err != nil {
		return ClusterView{}, err
	}
	switch view.Phase {
	case "Draining":
		return ClusterView{}, ErrClusterDraining
	case "Initializing":
		return ClusterView{}, ErrClusterInitializing
	}
	return s.Store.SetRunning(ctx, caller, req.Name, true)
}

func (s *Server) ControlPlaneDown(ctx context.Context, caller Caller, req NameRequest) (ClusterView, error) {
	if _, err := s.lookup(ctx, caller, req); err != nil {
		return ClusterView{}, err
	}
	return s.Store.SetRunning(ctx, caller, req.Name, false)
}

func (s *Server) DeleteCluster(ctx context.Context, caller Caller, req NameRequest) (ClusterView, error) {
	view, err := s.lookup(ctx, caller, req)
	if err != nil {
		return ClusterView{}, err
	}
	if view.Phase != "Draining" {
		s.revokeCerts(ctx, "deleteCluster", caller, req.Name)
	}
	return s.Store.Delete(ctx, caller, req.Name)
}

func (s *Server) revokeCerts(ctx context.Context, verb string, caller Caller, cluster string) {
	if s.Certs == nil {
		return
	}
	n, err := s.Certs.Revoke(ctx, string(caller), cluster)
	if err != nil {
		slog.Warn(verb+": the PKI revoke failed; the deny or the delete already ends access", "caller", string(caller), "name", cluster, "err", err)
		return
	}
	slog.Info(verb+": certificates revoked", "caller", string(caller), "name", cluster, "count", n)
}

func (s *Server) ListClusters(ctx context.Context, caller Caller) ([]ClusterView, error) {
	if caller == "" {
		return nil, ErrUnauthorized
	}
	return s.Store.List(ctx, caller)
}

type RevokeRequest struct {
	DID string

	Name string
}

type RevokeResponse struct {
	Revoked []revocation.ReplyEntry
}

func (s *Server) RevokeIdentity(ctx context.Context, caller Caller, req RevokeRequest) (*RevokeResponse, error) {
	if caller == "" {
		return nil, ErrUnauthorized
	}
	if req.DID != "" && req.DID != string(caller) {
		return nil, ErrForbidden
	}
	if s.Revoker == nil {
		return nil, ErrNoDenyWriter
	}

	if err := s.Revoker.WriteDeny(ctx, string(caller)); err != nil {
		return nil, fmt.Errorf("revokeIdentity: write the deny: %w", err)
	}

	observed, err := s.Revoker.FanOut(ctx, string(caller))
	if err != nil {
		return nil, fmt.Errorf("revokeIdentity: %w", err)
	}
	if req.Name != "" {
		scoped := make([]revocation.ClusterView, 0, 1)
		for _, c := range observed {
			if c.Name == req.Name {
				scoped = append(scoped, c)
			}
		}
		if len(scoped) == 0 {
			return nil, ErrClusterNotFound
		}
		observed = scoped
	}
	for _, c := range observed {
		if c.Name != "" {
			s.revokeCerts(ctx, "revokeIdentity", caller, c.Name)
		}
	}

	return &RevokeResponse{Revoked: revocation.Reply(revocation.Plan(observed))}, nil
}

func (s *Server) lookup(ctx context.Context, caller Caller, req NameRequest) (ClusterView, error) {
	if caller == "" {
		return ClusterView{}, ErrUnauthorized
	}
	if req.Name == "" {
		return ClusterView{}, ErrInvalidRequest
	}
	view, ok, err := s.Store.Get(ctx, caller, req.Name)
	if err != nil {
		return ClusterView{}, err
	}
	if !ok {
		return ClusterView{}, ErrClusterNotFound
	}
	return view, nil
}

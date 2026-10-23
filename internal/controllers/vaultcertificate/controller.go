package vaultcertificate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

const (
	Name = "vaultcertificate"

	Export = "applications"

	VaultExport = "hostprocesses"

	ConditionReady = "Ready"

	ReasonObserved = "Observed"

	ReasonWaiting = "Waiting"

	ReasonSourceUnavailable = "SourceUnavailable"

	ReasonSourceError = "SourceError"

	DefaultRefresh = 30 * time.Second
)

type Source interface {
	CertificateMetadata(ctx context.Context, server, mount, role, commonName string) (statusstate.VaultCertificateObservation, error)
}

var _ Source = (*statusstate.OpenBaoMetadataSource)(nil)

type Options struct {
	Source Source

	Scope func(workspace string) bool

	Refresh time.Duration
}

type Controller struct {
	source Source

	refresh time.Duration

	refs *binding.Table[struct{}]
}

func New(opts Options) *Controller {
	return &Controller{source: opts.Source, refresh: opt.Positive(opts.Refresh, DefaultRefresh), refs: binding.New[struct{}]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if err := m.AddToScheme(v1alpha1.AddToScheme); err != nil {
		return nil, err
	}
	ctl := New(opts)
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Gone(ctl.gone),
		ctlruntime.WatchesIn(VaultExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.OpenBaoServer) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, v1alpha1.KindOpenBaoServer, obj.Name))
		}),
		ctlruntime.WatchesIn(VaultExport, func(_ context.Context, src ctlruntime.Request, obj *v1alpha1.RemoteOpenBao) []ctlruntime.Request {
			return ctl.users(binding.Key(src.Workspace, v1alpha1.KindRemoteOpenBao, obj.Name))
		})}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.VaultCertificate](m, Name, ctl, options...); err != nil {
		return nil, fmt.Errorf("vaultcertificate: registering the VaultCertificate controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *v1alpha1.VaultCertificate) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.refs.Set(target, nil)
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := v1alpha1.VaultCertificateStatus{ObservedGeneration: generation, Conditions: object.Status.Conditions}
	server := v1alpha1.VaultServer(&object.Spec.Vault, "")
	mount := strings.TrimSpace(object.Spec.Mount)
	role := strings.TrimSpace(object.Spec.Role)
	commonName := strings.TrimSpace(object.Spec.CommonName)
	c.refs.Set(target, &struct{}{}, serverKey(req.Workspace, object.Spec.Vault))
	if server == "" || mount == "" || role == "" || commonName == "" {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonWaiting, "the VaultCertificate needs a server, a mount, a role and a commonName")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	if c.source == nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceUnavailable, "no vault source is configured")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	observation, err := c.source.CertificateMetadata(ctx, server, mount, role, commonName)
	if err != nil {
		conds.Set(&status.Conditions, generation, ConditionReady, false, ReasonSourceError, "reading the certificate metadata: "+err.Error())
		return ctlruntime.Result{}, errors.Join(fmt.Errorf("vaultcertificate: reading %s on %s: %w", commonName, server, err), c.apply(ctx, req, object, status))
	}
	status.Exists = observation.Exists
	status.SerialNumber = observation.SerialNumber
	status.Fingerprint = observation.Fingerprint
	status.ExpiresAt = observation.ExpiresAt
	status.RotationDue = observation.RotationDue
	conds.Set(&status.Conditions, generation, ConditionReady, true, ReasonObserved, "the certificate metadata is observed without private keys")
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{RequeueAfter: c.refresh}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *v1alpha1.VaultCertificate, status v1alpha1.VaultCertificateStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) gone(_ context.Context, req ctlruntime.Request) {
	c.refs.Set(binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}, nil)
}

func (c *Controller) users(ref string) []ctlruntime.Request {
	var out []ctlruntime.Request
	for _, t := range c.refs.Users(ref) {
		out = append(out, ctlruntime.Request{Cluster: t.Cluster, Workspace: t.Workspace, Name: t.Name})
	}
	return out
}

func serverKey(workspace string, ref commonv1.ObjectRef) string {
	if strings.TrimSpace(ref.Name) == "" {
		return ""
	}
	kind := v1alpha1.KindOpenBaoServer
	if strings.EqualFold(strings.TrimSpace(ref.Kind), v1alpha1.KindRemoteOpenBao) {
		kind = v1alpha1.KindRemoteOpenBao
	}
	return binding.Key(ref.InWorkspace(workspace), kind, ref.Name)
}

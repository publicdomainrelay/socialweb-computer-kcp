package remotepds

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	vaultctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/remote"
)

const (
	Name = "remotepds"

	Export = "pds"

	ConditionReady = "Ready"

	ConditionFailed = "Failed"

	DefaultTimeout = 10 * time.Second

	DefaultCheckInterval = 30 * time.Second
)

type SecretRefsFunc func(workspace string) remote.Secrets

type Options struct {
	Log *slog.Logger

	Bus *bus.Bus

	SecretRefs SecretRefsFunc

	Timeout time.Duration

	CheckInterval time.Duration
}

type Controller struct {
	log *slog.Logger

	roots *vaultctl.Roots

	bus *bus.Bus

	secretRefs SecretRefsFunc

	timeout time.Duration

	checkInterval time.Duration

	hosts *binding.Table[*remote.Host]
}

type transientError struct {
	err error
}

func (e transientError) Error() string { return e.err.Error() }

func (e transientError) Unwrap() error { return e.err }

func New(opts Options) *Controller {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	timeout := opts.Timeout
	timeout = opt.Positive(timeout, DefaultTimeout)
	checkInterval := opts.CheckInterval
	checkInterval = ctlruntime.Period(checkInterval, DefaultCheckInterval)
	return &Controller{log: log.With("component", "remotepds-controller"), roots: vaultctl.NewRoots(opts.Bus, log), bus: opts.Bus,
		secretRefs: opts.SecretRefs, timeout: timeout, checkInterval: checkInterval, hosts: binding.New[*remote.Host]()}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl := New(opts)
	ctl.Provide()
	if err := ctlruntime.For[*pdsv1alpha1.RemotePDS](m, Name, ctl, ctlruntime.Export(Export)); err != nil {
		return nil, fmt.Errorf("remotepds: registering the RemotePDS controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS) (ctlruntime.Result, error) {
	target := binding.Target{Cluster: req.Cluster, Workspace: req.Workspace, Name: req.Name}
	if !object.DeletionTimestamp.IsZero() {
		c.hosts.Set(target, nil)
		c.roots.Drop(self(req, object))
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := object.Status
	status.Phase = pdsv1alpha1.RemotePDSPending
	status.Ready = false
	status.Endpoint = ""
	if object.Spec.Suspend {
		conds.Set(&status.Conditions, generation, ConditionReady, false, "Suspended", "the RemotePDS is suspended")
		conds.Set(&status.Conditions, generation, ConditionFailed, false, "Suspended", "the RemotePDS is suspended")
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	checkedAt := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	status.LastCheckedAt = &checkedAt
	host, endpoint, reason, err := c.connect(ctx, req, object)
	if err != nil {
		c.hosts.Set(target, nil)
		return c.fail(ctx, req, object, status, reason, err)
	}
	c.hosts.Set(target, &host)
	info, err := host.Describe(ctx)
	if err != nil {
		return c.fail(ctx, req, object, status, "DescribeFailed", transientError{err: err})
	}
	status.Phase = pdsv1alpha1.RemotePDSReady
	status.Ready = true
	status.Endpoint = endpoint
	message := fmt.Sprintf("the PDS at %s reports %s", endpoint, info.DID)
	conds.Set(&status.Conditions, generation, ConditionReady, true, "Ready", message)
	conds.Set(&status.Conditions, generation, ConditionFailed, false, "DescribeSucceeded", "the PDS answered describeServer")
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	c.log.Info("the RemotePDS is ready", "cluster", req.Cluster, "workspace", req.Workspace, "name", req.Name,
		"endpoint", endpoint, "fieldManager", req.FieldManager())
	return ctlruntime.RefreshAfter(c.checkInterval), nil
}

func (c *Controller) fail(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS, status pdsv1alpha1.RemotePDSStatus,
	reason string, err error) (ctlruntime.Result, error) {
	generation := object.Generation
	status.Phase = pdsv1alpha1.RemotePDSFailed
	conds.Set(&status.Conditions, generation, ConditionReady, false, reason, err.Error())
	conds.Set(&status.Conditions, generation, ConditionFailed, true, reason, err.Error())
	if applyErr := c.apply(ctx, req, object, status); applyErr != nil {
		return ctlruntime.Result{}, errors.Join(err, applyErr)
	}
	var transient transientError
	if errors.As(err, &transient) {
		return ctlruntime.Result{}, fmt.Errorf("remotepds: %s: %w", reason, err)
	}
	c.log.Warn("the RemotePDS spec is invalid; it waits for a spec change", "workspace", req.Workspace, "name", req.Name,
		"reason", reason, "err", err.Error())
	return ctlruntime.Result{}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS, status pdsv1alpha1.RemotePDSStatus) error {
	next := object.DeepCopy()
	next.Status = status
	return req.ApplyStatus(ctx, next)
}

func (c *Controller) connect(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS) (*remote.Host, string, string, error) {
	baseURL, err := remoteURL(object.Spec.URL)
	if err != nil {
		return nil, "", "InvalidURL", err
	}
	didText := strings.TrimSpace(object.Spec.DID)
	if didText == "" {
		return nil, "", "InvalidDID", errors.New("the RemotePDS has no did")
	}
	if _, err := syntax.ParseDID(didText); err != nil {
		return nil, "", "InvalidDID", fmt.Errorf("the RemotePDS did %q is invalid", didText)
	}
	tlsConfig, err := c.tlsConfig(ctx, self(req, object), object.Spec.TLS, baseURL)
	if err != nil {
		return nil, "", "InvalidTLS", transientError{err: err}
	}
	var secrets remote.Secrets
	if c.secretRefs != nil {
		secrets = c.secretRefs(req.Workspace)
	}
	auth, err := remote.Credentials(ctx, object.Spec.Auth, c.vaultSecrets(self(req, object)))
	if err != nil {
		return nil, "", "InvalidAuth", transientError{err: err}
	}
	endpoint := strings.TrimRight(baseURL.String(), "/")
	host, err := remote.New(remote.Config{URL: endpoint, DID: didText, TLS: tlsConfig, Auth: auth, Secrets: secrets, Timeout: c.timeout})
	if err != nil {
		return nil, "", "InvalidConfig", err
	}
	return host, endpoint, "", nil
}

func remoteURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("the RemotePDS has no url")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("the RemotePDS url does not parse: %w", err)
	}
	if parsed.Host == "" {
		return nil, errors.New("the RemotePDS url has no host")
	}
	if parsed.User != nil {
		return nil, errors.New("the RemotePDS url must not carry user info")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, errors.New("the RemotePDS url needs an http or https scheme")
	}
	parsed.Scheme = scheme
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed, nil
}

func (c *Controller) tlsConfig(ctx context.Context, from commonv1.ObjectRef, spec pdsv1alpha1.RemotePDSTLS, baseURL *url.URL) (*tls.Config, error) {
	caSet := spec.CARef != nil || spec.CASecretRef != nil
	serverName := strings.TrimSpace(spec.ServerName)
	if !spec.Enabled {
		if caSet || serverName != "" {
			return nil, errors.New("tls fields are set but tls.enabled is false")
		}
		return nil, nil
	}
	if baseURL.Scheme != "https" {
		return nil, errors.New("tls is enabled but the RemotePDS url is not https")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if serverName != "" {
		config.ServerName = serverName
	}
	pool, err := c.roots.Pool(ctx, from, spec.CARef, spec.CASecretRef)
	if err != nil {
		return nil, err
	}
	if pool != nil {
		config = pool.ClientConfig(config)
	}
	return config, nil
}

func self(req ctlruntime.Request, object *pdsv1alpha1.RemotePDS) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: pdsv1alpha1.GroupName, Kind: Kind, Name: object.Name, Workspace: req.Workspace}
}

func (c *Controller) vaultSecrets(from commonv1.ObjectRef) remote.Secrets {
	if c.bus == nil {
		return nil
	}
	return func(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
		return vaultctl.ReadSecret(ctx, c.bus, from, ref)
	}
}

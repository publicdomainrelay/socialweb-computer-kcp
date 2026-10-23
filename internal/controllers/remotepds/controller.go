package remotepds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
)

const (
	Name = "remotepds"

	Export = "pds"

	ConditionReady = "Ready"

	ConditionFailed = "Failed"

	DefaultTimeout = 10 * time.Second

	DefaultCheckInterval = 30 * time.Second
)

type CABundleFunc func(ref string) ([]byte, error)

type SecretsFunc func(workspace string) pdsclient.SecretResolver

type Options struct {
	Log *slog.Logger

	CABundle CABundleFunc

	Secrets SecretsFunc

	Timeout time.Duration

	CheckInterval time.Duration
}

type Controller struct {
	log *slog.Logger

	caBundle CABundleFunc

	secrets SecretsFunc

	timeout time.Duration

	checkInterval time.Duration
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
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	checkInterval := opts.CheckInterval
	if checkInterval <= 0 {
		checkInterval = DefaultCheckInterval
	}
	return &Controller{log: log.With("component", "remotepds-controller"), caBundle: opts.CABundle, secrets: opts.Secrets,
		timeout: timeout, checkInterval: checkInterval}
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	ctl := New(opts)
	if err := ctlruntime.For[*pdsv1alpha1.RemotePDS](m, Name, ctl, ctlruntime.Export(Export)); err != nil {
		return nil, fmt.Errorf("remotepds: registering the RemotePDS controller: %w", err)
	}
	return ctl, nil
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS) (ctlruntime.Result, error) {
	if object == nil || !object.DeletionTimestamp.IsZero() {
		return ctlruntime.Result{}, nil
	}
	generation := object.Generation
	status := object.Status
	status.Phase = pdsv1alpha1.RemotePDSPending
	status.Ready = false
	status.Endpoint = ""
	if object.Spec.Suspend {
		setCondition(&status, ConditionReady, false, "Suspended", "the RemotePDS is suspended", generation)
		setCondition(&status, ConditionFailed, false, "Suspended", "the RemotePDS is suspended", generation)
		return ctlruntime.Result{}, c.apply(ctx, req, object, status)
	}
	checkedAt := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	status.LastCheckedAt = &checkedAt
	pds, endpoint, reason, err := c.connect(req, object)
	if err != nil {
		return c.fail(ctx, req, object, status, reason, err)
	}
	info, err := pds.Describe(ctx)
	if err != nil {
		return c.fail(ctx, req, object, status, "DescribeFailed", transientError{err: err})
	}
	status.Phase = pdsv1alpha1.RemotePDSReady
	status.Ready = true
	status.Endpoint = endpoint
	message := fmt.Sprintf("the PDS at %s reports %s", endpoint, info.DID.String())
	setCondition(&status, ConditionReady, true, "Ready", message, generation)
	setCondition(&status, ConditionFailed, false, "DescribeSucceeded", "the PDS answered describeServer", generation)
	if err := c.apply(ctx, req, object, status); err != nil {
		return ctlruntime.Result{}, err
	}
	c.log.Info("the RemotePDS is ready", "cluster", req.Cluster, "workspace", req.Workspace, "name", req.Name,
		"endpoint", endpoint, "fieldManager", req.FieldManager())
	return ctlruntime.Result{RequeueAfter: c.checkInterval}, nil
}

func (c *Controller) fail(ctx context.Context, req ctlruntime.Request, object *pdsv1alpha1.RemotePDS, status pdsv1alpha1.RemotePDSStatus,
	reason string, err error) (ctlruntime.Result, error) {
	generation := object.Generation
	status.Phase = pdsv1alpha1.RemotePDSFailed
	setCondition(&status, ConditionReady, false, reason, err.Error(), generation)
	setCondition(&status, ConditionFailed, true, reason, err.Error(), generation)
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

func (c *Controller) connect(req ctlruntime.Request, object *pdsv1alpha1.RemotePDS) (pdsclient.PDS, string, string, error) {
	baseURL, err := remoteURL(object.Spec.URL)
	if err != nil {
		return nil, "", "InvalidURL", err
	}
	didText := strings.TrimSpace(object.Spec.DID)
	if didText == "" {
		return nil, "", "InvalidDID", errors.New("the RemotePDS has no did")
	}
	did, err := syntax.ParseDID(didText)
	if err != nil {
		return nil, "", "InvalidDID", fmt.Errorf("the RemotePDS did %q is invalid", didText)
	}
	tlsConfig, err := c.tlsConfig(object.Spec.TLS, baseURL)
	if err != nil {
		return nil, "", "InvalidTLS", transientError{err: err}
	}
	var secrets pdsclient.SecretResolver
	if c.secrets != nil {
		secrets = c.secrets(req.Workspace)
	}
	tokens, dpop, err := pdsclient.AuthSources(object.Spec.Auth, secrets)
	if err != nil {
		return nil, "", "InvalidAuth", transientError{err: err}
	}
	endpoint := strings.TrimRight(baseURL.String(), "/")
	pds, err := pdsclient.NewRemote(pdsclient.RemoteConfig{
		BaseURL: endpoint,
		DID:     did,
		TLS:     tlsConfig,
		Tokens:  tokens,
		DPoP:    dpop,
		Timeout: c.timeout,
	})
	if err != nil {
		return nil, "", "InvalidConfig", err
	}
	return pds, endpoint, "", nil
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

func (c *Controller) tlsConfig(spec pdsv1alpha1.RemotePDSTLS, baseURL *url.URL) (*tls.Config, error) {
	caRef := strings.TrimSpace(spec.CABundleRef)
	serverName := strings.TrimSpace(spec.ServerName)
	if !spec.Enabled {
		if caRef != "" || serverName != "" {
			return nil, errors.New("tls fields are set but tls.enabled is false")
		}
		return nil, nil
	}
	if baseURL.Scheme != "https" {
		return nil, errors.New("tls is enabled but the RemotePDS url is not https")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caRef != "" {
		if c.caBundle == nil {
			return nil, errors.New("tls.caBundleRef is set but no CA bundle resolver is configured")
		}
		pem, err := c.caBundle(caRef)
		if err != nil {
			return nil, fmt.Errorf("reading the CA bundle %q: %w", caRef, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("the CA bundle %q does not parse", caRef)
		}
		config.RootCAs = pool
	}
	if serverName != "" {
		config.ServerName = serverName
	}
	return config, nil
}

func setCondition(status *pdsv1alpha1.RemotePDSStatus, conditionType string, value bool, reason, message string, generation int64) {
	conditionStatus := metav1.ConditionFalse
	if value {
		conditionStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

package remoteopenbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

const (
	ConditionReachable = "Reachable"

	ConditionAuthenticated = "Authenticated"

	ConditionUnsealed = "Unsealed"

	ConditionAuthorized = "Authorized"

	DefaultTimeout = 10 * time.Second
)

type Prober struct {
	remote *openbao.Remote

	timeout time.Duration

	log *slog.Logger
}

func New(remote *openbao.Remote, timeout time.Duration, log *slog.Logger) *Prober {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Prober{remote: remote, timeout: opt.Positive(timeout, DefaultTimeout), log: log.With("component", "remoteopenbao")}
}

type probeError struct {
	reason string

	err error
}

func (e *probeError) Error() string {
	return e.err.Error()
}

func (e *probeError) Unwrap() error {
	return e.err
}

func failed(reason string, err error) error {
	return &probeError{reason: reason, err: err}
}

func reasonOf(err error) string {
	var probe *probeError
	if errors.As(err, &probe) {
		return probe.reason
	}
	switch {
	case errors.Is(err, openbao.ErrRemoteDisabled):
		return "RemoteDisabled"
	case errors.Is(err, openbao.ErrUnsupported):
		return "Unsupported"
	case errors.Is(err, openbao.ErrSuspended):
		return "Suspended"
	case errors.Is(err, openbao.ErrInvalidConfig):
		return "InvalidConfig"
	case errors.Is(err, openbao.ErrInvalidRef):
		return "InvalidRef"
	default:
		return "Failed"
	}
}

func (p *Prober) Check(ctx context.Context, workspace string, object *v1alpha1.RemoteOpenBao) (v1alpha1.RemoteOpenBaoStatus, error) {
	generation := object.Generation
	name := object.Name
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	status := object.Status
	status.Phase = v1alpha1.RemoteOpenBaoPhasePending
	status.Ready = false
	status.Endpoint = ""
	status.Address = ""
	status.Initialized = false
	status.Version = ""
	status.Sealed = false
	status.Message = ""
	conds.Set(&status.Conditions, generation, ConditionAuthenticated, false, "Unchecked", "the RemoteOpenBao has not been authenticated yet")
	conds.Set(&status.Conditions, generation, ConditionUnsealed, false, "Unchecked", "the RemoteOpenBao has not been checked for a seal")
	conds.Set(&status.Conditions, generation, ConditionAuthorized, false, "Unchecked", "the RemoteOpenBao has not been authorized")
	if object.Spec.Suspend {
		status.Phase = v1alpha1.RemoteOpenBaoPhaseSuspended
		conds.Set(&status.Conditions, generation, ConditionReachable, false, "Suspended", "the RemoteOpenBao is suspended")
		conds.Set(&status.Conditions, generation, ConditionAuthenticated, false, "Suspended", "the RemoteOpenBao is suspended")
		conds.Set(&status.Conditions, generation, ConditionUnsealed, false, "Suspended", "the RemoteOpenBao is suspended")
		conds.Set(&status.Conditions, generation, ConditionAuthorized, false, "Suspended", "the RemoteOpenBao is suspended")
		status.Message = "the RemoteOpenBao is suspended"
		return status, nil
	}
	checkedAt := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	status.LastCheckedAt = &checkedAt
	if p.remote == nil {
		return p.failure(name, status, generation, ConditionReachable,
			errors.New("remoteopenbao: no RemoteOpenBao options is configured"))
	}
	target, err := p.remote.Target(ctx, workspace, object)
	if err != nil {
		return p.failure(name, status, generation, ConditionReachable, err)
	}
	status.Endpoint = target.URL
	status.Address = target.URL
	health, err := probeHealth(ctx, target)
	if err != nil {
		return p.failure(name, status, generation, ConditionReachable, failed("HealthFailed", err))
	}
	status.Version = health.Version
	status.Sealed = health.Sealed
	status.Initialized = health.Initialized
	conds.Set(&status.Conditions,
		generation, ConditionReachable, true, "Reachable",
		fmt.Sprintf("the OpenBao at %s answered sys/health", target.URL))

	seal, err := probeSealStatus(ctx, target)
	if err != nil {
		return p.failure(name, status, generation, ConditionUnsealed, failed("SealStatusFailed", err))
	}
	if seal.Version != "" {
		status.Version = seal.Version
	}
	status.Sealed = seal.Sealed
	if health.Sealed || seal.Sealed {
		message := fmt.Sprintf("the OpenBao at %s is sealed", target.URL)
		status.Phase = v1alpha1.RemoteOpenBaoPhaseFailed
		status.Ready = false
		status.Message = message
		conds.Set(&status.Conditions, generation, ConditionUnsealed, false, "Sealed", message)
		conds.Set(&status.Conditions, generation, ConditionAuthenticated, false, "Sealed", message)
		conds.Set(&status.Conditions, generation, ConditionAuthorized, false, "Sealed", message)
		p.log.Warn("checking a RemoteOpenBao", "name", name, "reason", "Sealed", "err", message)
		return status, errors.New(message)
	}
	conds.Set(&status.Conditions,
		generation, ConditionUnsealed, true, "Unsealed",
		fmt.Sprintf("the OpenBao at %s is unsealed", target.URL))

	inspector, err := p.remote.Open(ctx, workspace, object)
	if err != nil {
		return p.failure(name, status, generation, ConditionAuthenticated, err)
	}
	if err := inspector.Authenticated(ctx); err != nil {
		return p.failure(name, status, generation, ConditionAuthenticated, failed("AuthenticationFailed", err))
	}
	conds.Set(&status.Conditions,
		generation, ConditionAuthenticated, true, "Authenticated",
		"the RemoteOpenBao token answered auth/token/lookup-self")

	if err := checkMounts(ctx, inspector, object.Spec.Mounts); err != nil {
		return p.failure(name, status, generation, ConditionAuthorized, err)
	}
	if err := checkCapabilities(ctx, inspector, object.Spec.Capabilities); err != nil {
		return p.failure(name, status, generation, ConditionAuthorized, err)
	}
	status.Phase = v1alpha1.RemoteOpenBaoPhaseReady
	status.Ready = true
	conds.Set(&status.Conditions,
		generation, ConditionAuthorized, true, "Authorized",
		"the RemoteOpenBao answered with its declared mounts and capabilities")

	status.Message = fmt.Sprintf("the OpenBao at %s reports %s", target.URL, versionOrUnknown(status.Version))
	return status, nil
}

func (p *Prober) failure(name string, status v1alpha1.RemoteOpenBaoStatus, generation int64, condition string, err error) (v1alpha1.RemoteOpenBaoStatus, error) {
	reason := reasonOf(err)
	status.Phase = v1alpha1.RemoteOpenBaoPhaseFailed
	status.Ready = false
	status.Message = err.Error()
	conds.Set(&status.Conditions, generation, condition, false, reason, err.Error())
	p.log.Warn("checking a RemoteOpenBao", "name", name, "reason", reason, "err", err.Error())
	return status, err
}

func checkMounts(ctx context.Context, inspector *openbao.Client, mounts v1alpha1.RemoteOpenBaoMounts) error {
	if path := strings.TrimSpace(mounts.KV); path != "" {
		kind, found, err := inspector.MountType(ctx, path)
		if err != nil {
			return failed("MountsFailed", fmt.Errorf("reading the KV mount %s: %w", path, err))
		}
		if !found {
			return failed("MountMissing", fmt.Errorf("the KV mount %s is not mounted", path))
		}
		if kind != "kv" && kind != "generic" {
			return failed("MountType", fmt.Errorf("the KV mount %s is a %s mount", path, kind))
		}
	}
	if path := strings.TrimSpace(mounts.PKI); path != "" {
		kind, found, err := inspector.MountType(ctx, path)
		if err != nil {
			return failed("MountsFailed", fmt.Errorf("reading the PKI mount %s: %w", path, err))
		}
		if !found {
			return failed("MountMissing", fmt.Errorf("the PKI mount %s is not mounted", path))
		}
		if kind != "pki" {
			return failed("MountType", fmt.Errorf("the PKI mount %s is a %s mount", path, kind))
		}
	}
	if path := strings.TrimSpace(mounts.Auth); path != "" {
		_, found, err := inspector.AuthMountType(ctx, path)
		if err != nil {
			return failed("MountsFailed", fmt.Errorf("reading the auth mount %s: %w", path, err))
		}
		if !found {
			return failed("MountMissing", fmt.Errorf("the auth mount %s is not mounted", path))
		}
	}
	return nil
}

func checkCapabilities(ctx context.Context, inspector *openbao.Client, capabilities []string) error {
	if len(capabilities) == 0 {
		return nil
	}
	answers, err := inspector.Capabilities(ctx, capabilities)
	if err != nil {
		return failed("CapabilitiesFailed", err)
	}
	for _, path := range capabilities {
		allowed := false
		for _, capability := range answers[path] {
			if capability != "deny" {
				allowed = true
				break
			}
		}
		if !allowed {
			return failed("CapabilityDenied", fmt.Errorf("the token holds no capability on %s", path))
		}
	}
	return nil
}

type healthResponse struct {
	Initialized bool `json:"initialized"`

	Sealed bool `json:"sealed"`

	Version string `json:"version"`
}

type sealStatusResponse struct {
	Initialized bool `json:"initialized"`

	Sealed bool `json:"sealed"`

	Version string `json:"version"`
}

func probeHealth(ctx context.Context, target openbao.Target) (healthResponse, error) {
	var out healthResponse
	body, status, err := get(ctx, target, "sys/health")
	if err != nil {
		return out, err
	}
	switch status {
	case http.StatusOK, http.StatusTooManyRequests, 472, 473, http.StatusNotImplemented, http.StatusServiceUnavailable:
	default:
		return out, fmt.Errorf("GET /v1/sys/health -> HTTP %d", status)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("GET /v1/sys/health returned a body that is not JSON: %v", err)
	}
	return out, nil
}

func probeSealStatus(ctx context.Context, target openbao.Target) (sealStatusResponse, error) {
	var out sealStatusResponse
	body, status, err := get(ctx, target, "sys/seal-status")
	if err != nil {
		return out, err
	}
	if status < 200 || status > 299 {
		return out, fmt.Errorf("GET /v1/sys/seal-status -> HTTP %d", status)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("GET /v1/sys/seal-status returned a body that is not JSON: %v", err)
	}
	return out, nil
}

func get(ctx context.Context, target openbao.Target, path string) ([]byte, int, error) {
	url := strings.TrimRight(target.URL, "/") + "/v1/" + strings.TrimPrefix(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := target.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, 0, err
	}
	return body, resp.StatusCode, nil
}

func versionOrUnknown(version string) string {
	if strings.TrimSpace(version) == "" {
		return "an unknown version"
	}
	return version
}

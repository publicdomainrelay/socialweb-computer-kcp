package remoteopenbao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultaccess"
)

const statusUpdateAttempts = 5

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
	case errors.Is(err, vaultaccess.ErrRemoteDisabled):
		return "RemoteDisabled"
	case errors.Is(err, vaultaccess.ErrUnsupported):
		return "Unsupported"
	case errors.Is(err, vaultaccess.ErrSuspended):
		return "Suspended"
	case errors.Is(err, vaultaccess.ErrInvalidConfig):
		return "InvalidConfig"
	case errors.Is(err, vaultaccess.ErrInvalidRef):
		return "InvalidRef"
	default:
		return "Failed"
	}
}

func (c *Controller) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil || u.GetDeletionTimestamp() != nil {
		return nil
	}
	if dyn == nil {
		return errors.New("remoteopenbao: a dynamic client is required to reconcile")
	}
	var object v1alpha1.RemoteOpenBao
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &object); err != nil {
		return fmt.Errorf("remoteopenbao: decoding %s: %w", u.GetName(), err)
	}
	generation := object.Generation
	name := object.Name
	if c.settled(object.Status, generation) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	status := object.Status
	status.Phase = v1alpha1.RemoteOpenBaoPhasePending
	status.Ready = false
	status.Endpoint = ""
	status.Version = ""
	status.Sealed = false
	status.Message = ""
	setCondition(&status, ConditionAuthenticated, false, "Unchecked", "the RemoteOpenBao has not been authenticated yet", generation)
	setCondition(&status, ConditionUnsealed, false, "Unchecked", "the RemoteOpenBao has not been checked for a seal", generation)
	setCondition(&status, ConditionAuthorized, false, "Unchecked", "the RemoteOpenBao has not been authorized", generation)
	if object.Spec.Suspend {
		status.Phase = v1alpha1.RemoteOpenBaoPhaseSuspended
		setCondition(&status, ConditionReachable, false, "Suspended", "the RemoteOpenBao is suspended", generation)
		setCondition(&status, ConditionAuthenticated, false, "Suspended", "the RemoteOpenBao is suspended", generation)
		setCondition(&status, ConditionUnsealed, false, "Suspended", "the RemoteOpenBao is suspended", generation)
		setCondition(&status, ConditionAuthorized, false, "Suspended", "the RemoteOpenBao is suspended", generation)
		status.Message = "the RemoteOpenBao is suspended"
		return c.writeStatus(ctx, dyn, u, status)
	}
	if c.waiting(name, generation) {
		return nil
	}
	checkedAt := metav1.NewTime(c.now().UTC().Truncate(time.Second))
	status.LastCheckedAt = &checkedAt
	if c.resolver == nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionReachable,
			errors.New("remoteopenbao: no vaultaccess resolver is configured"))
	}
	target, err := c.resolver.RemoteTarget(&object)
	if err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionReachable, err)
	}
	status.Endpoint = target.URL
	health, err := probeHealth(ctx, target)
	if err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionReachable, failed("HealthFailed", err))
	}
	status.Version = health.Version
	status.Sealed = health.Sealed
	setCondition(&status, ConditionReachable, true, "Reachable",
		fmt.Sprintf("the OpenBao at %s answered sys/health", target.URL), generation)
	seal, err := probeSealStatus(ctx, target)
	if err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionUnsealed, failed("SealStatusFailed", err))
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
		setCondition(&status, ConditionUnsealed, false, "Sealed", message, generation)
		setCondition(&status, ConditionAuthenticated, false, "Sealed", message, generation)
		setCondition(&status, ConditionAuthorized, false, "Sealed", message, generation)
		c.fail(name, generation)
		c.log.Warn("checking a RemoteOpenBao", "name", name, "reason", "Sealed", "err", message)
		return c.writeStatus(ctx, dyn, u, status)
	}
	setCondition(&status, ConditionUnsealed, true, "Unsealed",
		fmt.Sprintf("the OpenBao at %s is unsealed", target.URL), generation)
	session, err := c.resolver.Resolve(ctx, vaultaccess.RemoteRef{Remote: &object})
	if err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionAuthenticated, err)
	}
	inspector, ok := session.(vaultaccess.RemoteInspector)
	if !ok {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionAuthenticated,
			failed("Unsupported", errors.New("remoteopenbao: the vaultaccess session cannot inspect the OpenBao")))
	}
	if err := inspector.Authenticated(ctx); err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionAuthenticated, failed("AuthenticationFailed", err))
	}
	setCondition(&status, ConditionAuthenticated, true, "Authenticated",
		"the RemoteOpenBao token answered auth/token/lookup-self", generation)
	if err := checkMounts(ctx, inspector, object.Spec.Mounts); err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionAuthorized, err)
	}
	if err := checkCapabilities(ctx, inspector, object.Spec.Capabilities); err != nil {
		return c.writeFailure(ctx, dyn, u, &status, generation, ConditionAuthorized, err)
	}
	status.Phase = v1alpha1.RemoteOpenBaoPhaseReady
	status.Ready = true
	setCondition(&status, ConditionAuthorized, true, "Authorized",
		"the RemoteOpenBao answered with its declared mounts and capabilities", generation)
	status.Message = fmt.Sprintf("the OpenBao at %s reports %s", target.URL, versionOrUnknown(status.Version))
	c.succeed(name)
	return c.writeStatus(ctx, dyn, u, status)
}

func (c *Controller) writeFailure(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured, status *v1alpha1.RemoteOpenBaoStatus, generation int64, condition string, err error) error {
	reason := reasonOf(err)
	status.Phase = v1alpha1.RemoteOpenBaoPhaseFailed
	status.Ready = false
	status.Message = err.Error()
	setCondition(status, condition, false, reason, err.Error(), generation)
	c.fail(u.GetName(), generation)
	c.log.Warn("checking a RemoteOpenBao", "name", u.GetName(), "reason", reason, "err", err.Error())
	return c.writeStatus(ctx, dyn, u, *status)
}

func (c *Controller) settled(status v1alpha1.RemoteOpenBaoStatus, generation int64) bool {
	condition := meta.FindStatusCondition(status.Conditions, ConditionReachable)
	if condition == nil || condition.ObservedGeneration < generation {
		return false
	}
	if status.Phase == v1alpha1.RemoteOpenBaoPhaseSuspended {
		return true
	}
	if status.LastCheckedAt == nil {
		return false
	}
	age := c.now().Sub(status.LastCheckedAt.Time)
	return age >= 0 && age < c.checkInterval
}

func checkMounts(ctx context.Context, inspector vaultaccess.RemoteInspector, mounts v1alpha1.RemoteOpenBaoMounts) error {
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

func checkCapabilities(ctx context.Context, inspector vaultaccess.RemoteInspector, capabilities []string) error {
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

func probeHealth(ctx context.Context, target vaultaccess.Target) (healthResponse, error) {
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

func probeSealStatus(ctx context.Context, target vaultaccess.Target) (sealStatusResponse, error) {
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

func get(ctx context.Context, target vaultaccess.Target, path string) ([]byte, int, error) {
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

func (c *Controller) writeStatus(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured, status v1alpha1.RemoteOpenBaoStatus) error {
	desired, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("remoteopenbao: encoding the status of %s: %w", u.GetName(), err)
	}
	if desired == nil {
		desired = map[string]any{}
	}
	name := u.GetName()
	ri := dyn.Resource(RemoteOpenBaoGVR)
	var current *unstructured.Unstructured
	for range statusUpdateAttempts {
		if current == nil {
			current, err = ri.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("remoteopenbao: reading %s: %w", name, err)
			}
		}
		if current.GetDeletionTimestamp() != nil {
			return nil
		}
		existing, _, err := unstructured.NestedMap(current.Object, "status")
		if err != nil {
			return fmt.Errorf("remoteopenbao: reading the status of %s: %w", name, err)
		}
		if reflect.DeepEqual(existing, desired) {
			return nil
		}
		next := current.DeepCopy()
		next.Object["status"] = desired
		_, err = ri.UpdateStatus(ctx, next, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return fmt.Errorf("remoteopenbao: updating the status of %s: %w", name, err)
		}
		current = nil
	}
	return fmt.Errorf("remoteopenbao: the status of %s stayed in conflict", name)
}

func setCondition(status *v1alpha1.RemoteOpenBaoStatus, conditionType string, value bool, reason, message string, generation int64) {
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

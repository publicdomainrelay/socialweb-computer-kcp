package remotepds

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

const statusUpdateAttempts = 5

func (c *Controller) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil || u.GetDeletionTimestamp() != nil {
		return nil
	}
	if dyn == nil {
		return errors.New("remotepds: a dynamic client is required to reconcile")
	}
	var object pdsv1alpha1.RemotePDS
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &object); err != nil {
		return fmt.Errorf("remotepds: decoding %s: %w", u.GetName(), err)
	}
	generation := object.Generation
	if c.settled(object.Status, generation) {
		return nil
	}
	status := object.Status
	status.Phase = pdsv1alpha1.RemotePDSPending
	status.Ready = false
	status.Endpoint = ""
	if object.Spec.Suspend {
		setCondition(&status, ConditionReady, false, "Suspended", "the RemotePDS is suspended", generation)
		setCondition(&status, ConditionFailed, false, "Suspended", "the RemotePDS is suspended", generation)
		return c.writeStatus(ctx, dyn, u, status)
	}
	checkedAt := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	status.LastCheckedAt = &checkedAt
	pds, endpoint, reason, err := c.connect(object)
	if err != nil {
		status.Phase = pdsv1alpha1.RemotePDSFailed
		setCondition(&status, ConditionReady, false, reason, err.Error(), generation)
		setCondition(&status, ConditionFailed, true, reason, err.Error(), generation)
		c.log.Warn("checking a RemotePDS", "name", object.Name, "reason", reason, "err", err.Error())
		return c.writeStatus(ctx, dyn, u, status)
	}
	info, err := pds.Describe(ctx)
	if err != nil {
		status.Phase = pdsv1alpha1.RemotePDSFailed
		setCondition(&status, ConditionReady, false, "DescribeFailed", err.Error(), generation)
		setCondition(&status, ConditionFailed, true, "DescribeFailed", err.Error(), generation)
		c.log.Warn("describing a RemotePDS", "name", object.Name, "err", err.Error())
		return c.writeStatus(ctx, dyn, u, status)
	}
	status.Phase = pdsv1alpha1.RemotePDSReady
	status.Ready = true
	status.Endpoint = endpoint
	message := fmt.Sprintf("the PDS at %s reports %s", endpoint, info.DID.String())
	setCondition(&status, ConditionReady, true, "Ready", message, generation)
	setCondition(&status, ConditionFailed, false, "DescribeSucceeded", "the PDS answered describeServer", generation)
	return c.writeStatus(ctx, dyn, u, status)
}

func (c *Controller) settled(status pdsv1alpha1.RemotePDSStatus, generation int64) bool {
	condition := meta.FindStatusCondition(status.Conditions, ConditionReady)
	if condition == nil || condition.ObservedGeneration < generation {
		return false
	}
	if status.LastCheckedAt == nil {
		return false
	}
	age := time.Since(status.LastCheckedAt.Time)
	return age >= 0 && age < c.checkInterval
}

func (c *Controller) connect(object pdsv1alpha1.RemotePDS) (pdsclient.PDS, string, string, error) {
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
		return nil, "", "InvalidTLS", err
	}
	tokens, dpop, err := pdsclient.AuthSources(object.Spec.Auth, c.secrets)
	if err != nil {
		return nil, "", "InvalidAuth", err
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

func (c *Controller) writeStatus(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured, status pdsv1alpha1.RemotePDSStatus) error {
	desired, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("remotepds: encoding the status of %s: %w", u.GetName(), err)
	}
	if desired == nil {
		desired = map[string]any{}
	}
	name := u.GetName()
	ri := dyn.Resource(RemotePDSGVR)
	var current *unstructured.Unstructured
	for attempt := 0; attempt < statusUpdateAttempts; attempt++ {
		if current == nil {
			current, err = ri.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("remotepds: reading %s: %w", name, err)
			}
		}
		if current.GetDeletionTimestamp() != nil {
			return nil
		}
		existing, _, err := unstructured.NestedMap(current.Object, "status")
		if err != nil {
			return fmt.Errorf("remotepds: reading the status of %s: %w", name, err)
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
			return fmt.Errorf("remotepds: updating the status of %s: %w", name, err)
		}
		current = nil
	}
	return fmt.Errorf("remotepds: the status of %s stayed in conflict", name)
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

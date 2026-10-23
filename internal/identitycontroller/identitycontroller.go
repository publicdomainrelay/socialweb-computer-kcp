package identitycontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
)

const (
	ComponentKind = "Pds"

	ComponentName = "pds"

	FieldManager = "identitycontroller"

	ConditionReady = "Ready"

	DefaultRetryAfter = 2 * time.Second

	requestTimeout = 5 * time.Second
)

var MarketIdentityGVR = v1alpha1.MarketIdentityGVR

var RecordWatchGVR = v1alpha1.AtprotoRecordWatchGVR

const (
	ConditionVerified = v1alpha1.ConditionVerified

	ReasonRevoked = "Revoked"

	specPatchAttempts = 5
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type EndpointFunc func(workspace, kind, name string) (string, bool)

type Options struct {
	Workspace string

	Dynamic DynamicFactory

	Endpoints EndpointFunc

	Log *slog.Logger

	HTTPClient *http.Client

	Resolver *pdsclient.Resolver

	RetryAfter time.Duration

	Vault SecretStore

	OAuth OAuthOptions
}

type Controller struct {
	workspace string

	dynamic DynamicFactory

	endpoints EndpointFunc

	log *slog.Logger

	http *http.Client

	resolver *pdsclient.Resolver

	retry time.Duration

	vault SecretStore

	oauth OAuthOptions

	states oauthclient.StateStore

	qrMu sync.Mutex

	qr map[string]*qrTransfer
}

func New(opts Options) (*Controller, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("identitycontroller: a workspace is required")
	}
	if opts.Dynamic == nil {
		return nil, fmt.Errorf("identitycontroller: a dynamic client factory is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	resolver := opts.Resolver
	if resolver == nil {
		resolver = &pdsclient.Resolver{HTTPClient: client, Timeout: requestTimeout}
	}
	retry := opts.RetryAfter
	if retry <= 0 {
		retry = DefaultRetryAfter
	}
	states := opts.OAuth.States
	if states == nil {
		if opts.Vault != nil && opts.OAuth.StateRef != nil {
			states = &VaultStateStore{Vault: opts.Vault, Ref: *opts.OAuth.StateRef}
		} else {
			states = &oauthclient.MemoryStateStore{}
		}
	}
	return &Controller{workspace: opts.Workspace, dynamic: opts.Dynamic, endpoints: opts.Endpoints, log: log.With("component", "identity-controller"),
		http: client, resolver: resolver, retry: retry, vault: opts.Vault, oauth: opts.OAuth, states: states, qr: map[string]*qrTransfer{}}, nil
}

func Run(ctx context.Context, opts Options) error {
	ctl, err := New(opts)
	if err != nil {
		return err
	}
	return ctl.Run(ctx)
}

func (c *Controller) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for ctx.Err() == nil {
		if err := c.watch(ctx); err != nil && ctx.Err() == nil {
			c.log.Warn("watching MarketIdentity objects", "workspace", c.workspace, "err", err.Error())
		}
		if !sleep(ctx, c.retry) {
			return nil
		}
	}
	return nil
}

func (c *Controller) watch(ctx context.Context) error {
	dyn, err := c.dynamic(c.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(MarketIdentityGVR)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		c.reconcile(ctx, dyn, &list.Items[i])
	}
	w, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer w.Stop()
	timer := time.NewTimer(c.retry)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case event, ok := <-w.ResultChan():
			if !ok {
				return nil
			}
			if event.Type == watch.Deleted || event.Type == watch.Bookmark {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil {
				continue
			}
			c.reconcile(ctx, dyn, u)
		}
	}
}

func (c *Controller) reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	if u == nil || u.GetDeletionTimestamp() != nil {
		return
	}
	ctx = bus.WithObject(ctx, c.workspace, u)
	var id v1alpha1.MarketIdentity
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &id); err != nil {
		c.log.Warn("decoding a MarketIdentity", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := id.Status
	did := strings.TrimSpace(status.Did)
	if suspended, _, _ := unstructured.NestedBool(u.Object, "spec", "suspend"); suspended {
		status.Phase = v1alpha1.MarketIdentityPhasePending
		status.ObservedGeneration = generation
		status.Message = "suspended"
		setReady(&status, generation, false, "Suspended", "the MarketIdentity is suspended")
		c.write(ctx, dyn, u.GetName(), status)
		return
	}
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		c.reconcileVerifiedAssociation(ctx, dyn, &id, generation, &status)
	}
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo && did != "" && status.ObservedGeneration >= generation &&
		status.Phase != "" && status.Phase != v1alpha1.MarketIdentityPhasePending {
		c.write(ctx, dyn, u.GetName(), status)
		return
	}
	switch id.Spec.Mode {
	case v1alpha1.MarketIdentityModeLocalRepo:
		c.reconcileLocalRepo(ctx, dyn, &id, generation, status)
	case v1alpha1.MarketIdentityModePdsAccount:
		c.reconcilePdsAccount(ctx, dyn, &id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthBrowser:
		c.reconcileOAuthBrowser(ctx, dyn, &id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthQr:
		c.reconcileOAuthQr(ctx, dyn, &id, generation, status)
	case v1alpha1.MarketIdentityModeOAuthDelegated:
		c.reconcileOAuthDelegated(ctx, dyn, &id, generation, status)
	case "":
		status.Phase = v1alpha1.MarketIdentityPhasePending
		status.ObservedGeneration = generation
		status.Message = "the MarketIdentity has no mode; no DID is recorded"
		setReady(&status, generation, false, "UnknownMode", status.Message)
		c.write(ctx, dyn, u.GetName(), status)
	default:
		status.Phase = v1alpha1.MarketIdentityPhasePending
		status.ObservedGeneration = generation
		status.Message = fmt.Sprintf("unknown mode %q; no DID is recorded", id.Spec.Mode)
		setReady(&status, generation, false, "UnknownMode", status.Message)
		c.write(ctx, dyn, u.GetName(), status)
	}
}

func (c *Controller) reconcileLocalRepo(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus) {
	endpoint := ""
	if c.endpoints != nil {
		if url, ok := c.endpoints(c.workspace, ComponentKind, ComponentName); ok {
			endpoint = strings.TrimSpace(url)
		}
	}
	if endpoint == "" {
		c.pending(ctx, dyn, id, generation, status, "PdsNotReady", "waiting for the Pds/pds component endpoint in workspace "+c.workspace)
		return
	}
	did, err := c.resolveDID(ctx, endpoint)
	if err != nil {
		c.pending(ctx, dyn, id, generation, status, "PdsNotReady", "waiting for the PDS at "+endpoint+": "+err.Error())
		return
	}
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.ObservedGeneration = generation
	if strings.TrimSpace(status.Did) == "" {
		status.Did = did
	}
	if handle := strings.TrimSpace(id.Spec.Handle); handle != "" {
		status.Handle = handle
	}
	setAssociated(&status, generation, id.Spec)
	if id.Spec.Association != v1alpha1.AssociationSkip && !associatedTrue(&status) {
		status.Phase = v1alpha1.MarketIdentityPhaseAwaitingApproval
		status.Message = "the PDS is ready; waiting for the operator's verified association claim"
		setReady(&status, generation, false, "AwaitingAssociation", status.Message)
		c.write(ctx, dyn, id.GetName(), status)
		return
	}
	status.Phase = v1alpha1.MarketIdentityPhaseReady
	status.Message = ""
	setReady(&status, generation, true, "Ready", "the PDS at "+endpoint+" reports "+did)
	c.write(ctx, dyn, id.GetName(), status)
}

func (c *Controller) reconcileVerifiedAssociation(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity,
	generation int64, status *v1alpha1.MarketIdentityStatus) {
	if id.Spec.Association == v1alpha1.AssociationSkip {
		return
	}
	watchName := strings.TrimSpace(id.Spec.AssociationWatch)
	if watchName == "" {
		return
	}
	subject := strings.TrimSpace(status.Did)
	if subject == "" {
		return
	}
	u, err := dyn.Resource(RecordWatchGVR).Get(ctx, watchName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		c.log.Warn("reading the association watch", "workspace", c.workspace, "name", watchName, "err", err.Error())
		return
	}
	var watchObject v1alpha1.AtprotoRecordWatch
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &watchObject); err != nil {
		c.log.Warn("decoding the association watch", "workspace", c.workspace, "name", watchName, "err", err.Error())
		return
	}
	verified := false
	if condition := meta.FindStatusCondition(watchObject.Status.Conditions, ConditionVerified); condition != nil {
		verified = condition.Status == metav1.ConditionTrue
	}
	if !verified && len(watchObject.Status.Deleted) == 0 {
		return
	}
	approved := map[string]bool{}
	for _, record := range watchObject.Status.Records {
		did := associationClaimDid(record, subject, id.Spec.AssociationService)
		if did == "" {
			continue
		}
		if operator := strings.TrimSpace(id.Spec.AssociateWith); strings.HasPrefix(operator, "did:") && did != operator {
			continue
		}
		approved[did] = true
	}
	next := make([]string, 0, len(id.Spec.ApprovedAssociations)+len(approved))
	for _, did := range id.Spec.ApprovedAssociations {
		if approved[did] {
			next = append(next, did)
			delete(approved, did)
		}
	}
	fresh := make([]string, 0, len(approved))
	for did := range approved {
		fresh = append(fresh, did)
	}
	slices.Sort(fresh)
	next = append(next, fresh...)
	removed := len(next) < len(id.Spec.ApprovedAssociations)
	if !slices.Equal(next, id.Spec.ApprovedAssociations) {
		if err := c.patchApprovedAssociations(ctx, dyn, id.Name, next); err != nil {
			c.log.Warn("writing the approved associations", "workspace", c.workspace, "name", id.Name, "err", err.Error())
			return
		}
		id.Spec.ApprovedAssociations = next
	}
	status.Associations = append([]string(nil), next...)
	if removed {
		message := "the operator's verified claim record is gone"
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionFalse,
			Reason: ReasonRevoked, Message: message, ObservedGeneration: generation})
		return
	}
	setAssociated(status, generation, id.Spec)
}

func associationClaimDid(record v1alpha1.WatchedRecord, subject, service string) string {
	var value struct {
		Challenge string `json:"challenge"`
		KeyID     string `json:"keyId"`
		Service   string `json:"service"`
	}
	if err := json.Unmarshal(record.Value, &value); err != nil {
		return ""
	}
	challenge := strings.TrimSpace(value.Challenge)
	keyID := strings.TrimSpace(value.KeyID)
	if strings.TrimSpace(service) == "" || strings.TrimSpace(value.Service) != service {
		return ""
	}
	if strings.HasPrefix(challenge, "did:") && keyID == subject && record.Did == challenge {
		return challenge
	}
	return ""
}

func (c *Controller) patchApprovedAssociations(ctx context.Context, dyn dynamic.Interface, name string, approved []string) error {
	if approved == nil {
		approved = []string{}
	}
	ri := dyn.Resource(MarketIdentityGVR)
	for range specPatchAttempts {
		current, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		body, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/resourceVersion", "value": current.GetResourceVersion()},
			{"op": "add", "path": "/spec/approvedAssociations", "value": approved},
		})
		if err != nil {
			return err
		}
		_, err = ri.Patch(ctx, name, types.JSONPatchType, body, metav1.PatchOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsInvalid(err) {
			return err
		}
	}
	return fmt.Errorf("the approved associations of MarketIdentity %s stayed in conflict", name)
}

func (c *Controller) pending(ctx context.Context, dyn dynamic.Interface, id *v1alpha1.MarketIdentity, generation int64, status v1alpha1.MarketIdentityStatus, reason, message string) {
	status.Phase = v1alpha1.MarketIdentityPhasePending
	status.ObservedGeneration = generation
	status.Message = message
	setReady(&status, generation, false, reason, message)
	c.write(ctx, dyn, id.GetName(), status)
}

func (c *Controller) resolveDID(ctx context.Context, endpoint string) (string, error) {
	pds, err := c.pdsAt(endpoint)
	if err != nil {
		return "", err
	}
	info, err := pds.Describe(ctx)
	if err != nil {
		return "", err
	}
	did := strings.TrimSpace(info.DID.String())
	if did == "" {
		return "", fmt.Errorf("the PDS reported no did")
	}
	return did, nil
}

func (c *Controller) pdsAt(endpoint string) (pdsclient.PDS, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("the PDS has no endpoint")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	if c.resolver != nil && c.resolver.Handlers != nil {
		return c.resolver.ResolveLocal(pdsclient.LocalRef{Workspace: c.workspace, Kind: ComponentKind, Name: ComponentName, Endpoint: endpoint})
	}
	r := c.resolver
	if r == nil {
		r = &pdsclient.Resolver{HTTPClient: c.http, Timeout: requestTimeout}
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	return pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, HTTPClient: r.HTTPClient, TLS: r.TLS, Tokens: r.Tokens, DPoP: r.DPoP, Timeout: timeout})
}

func (c *Controller) write(ctx context.Context, dyn dynamic.Interface, name string, status v1alpha1.MarketIdentityStatus) {
	if err := c.writeStatus(ctx, dyn, name, status); err != nil && ctx.Err() == nil {
		c.log.Warn("writing the MarketIdentity status", "workspace", c.workspace, "name", name, "err", err.Error())
	}
}

func (c *Controller) writeStatus(ctx context.Context, dyn dynamic.Interface, name string, status v1alpha1.MarketIdentityStatus) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	if m == nil {
		m = map[string]any{}
	}
	ri := dyn.Resource(MarketIdentityGVR)
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.GetDeletionTimestamp() != nil {
			return nil
		}
		existing, _, _ := unstructured.NestedMap(cur.Object, "status")
		if reflect.DeepEqual(existing, m) {
			return nil
		}
		cur.Object["status"] = m
		_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("the status of MarketIdentity %s stayed in conflict", name)
}

func setReady(status *v1alpha1.MarketIdentityStatus, generation int64, ready bool, reason, message string) {
	value := metav1.ConditionFalse
	if ready {
		value = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: ConditionReady, Status: value, Reason: reason, Message: message, ObservedGeneration: generation})
}

func associatedTrue(status *v1alpha1.MarketIdentityStatus) bool {
	condition := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionAssociated)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func setAssociated(status *v1alpha1.MarketIdentityStatus, generation int64, spec v1alpha1.MarketIdentitySpec) {
	associations := make([]string, 0, len(spec.ApprovedAssociations))
	for _, did := range spec.ApprovedAssociations {
		did = strings.TrimSpace(did)
		if did == "" || slices.Contains(associations, did) {
			continue
		}
		associations = append(associations, did)
	}
	status.Associations = associations
	if spec.Association == v1alpha1.AssociationSkip {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionTrue, Reason: "AssociationSkipped", Message: "the association is skipped", ObservedGeneration: generation})
		return
	}
	if len(status.Associations) > 0 {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionTrue, Reason: "Associated", Message: "the operator association is approved", ObservedGeneration: generation})
		return
	}
	if revoked := meta.FindStatusCondition(status.Conditions, v1alpha1.ConditionAssociated); revoked != nil && revoked.Reason == ReasonRevoked {
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionFalse,
			Reason: ReasonRevoked, Message: revoked.Message, ObservedGeneration: generation})
		return
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{Type: v1alpha1.ConditionAssociated, Status: metav1.ConditionFalse, Reason: "AwaitingAssociation", Message: "the PDS is ready; the association is pending", ObservedGeneration: generation})
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

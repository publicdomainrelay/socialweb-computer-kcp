package statusstate

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	FieldManager = "statusstate"

	ConditionReady = "Ready"

	ConditionInSync = "InSync"

	ReasonReady = "Observed"

	ReasonWaiting = "Waiting"

	ReasonSourceUnavailable = "SourceUnavailable"

	ReasonSourceError = "SourceError"

	ReasonNotFound = "NotFound"

	DefaultRetryAfter = 2 * time.Second

	DefaultResync = 30 * time.Second

	maxMessage = 512
)

var (
	AtprotoRepoGVR = v1alpha1.AtprotoRepoGVR

	PdsAccountGVR = v1alpha1.PdsAccountGVR

	AtprotoSessionGVR = v1alpha1.AtprotoSessionGVR

	PlcIdentityGVR = v1alpha1.PlcIdentityGVR

	RelayStatusGVR = v1alpha1.RelayStatusGVR

	VaultCredentialGVR = v1alpha1.VaultCredentialGVR

	VaultCertificateGVR = v1alpha1.VaultCertificateGVR
)

type DynamicFactory func(workspace string) (dynamic.Interface, error)

type Options struct {
	Workspace string

	Dynamic DynamicFactory

	PDS PDSResolver

	RepoResolver *pdsclient.RepoResolver

	Accounts AccountSource

	Sessions SessionSource

	PLC PLCClient

	PLCBaseURL string

	PLCHandlers *component.HandlerRegistry

	Relay RelaySource

	RelayEndpoint RelayEndpointFunc

	RelayHandlers *component.HandlerRegistry

	Vault VaultSource

	Log *slog.Logger

	RetryAfter time.Duration

	Resync time.Duration
}

type Controller struct {
	workspace string

	dynamic DynamicFactory

	pds PDSResolver

	repo *pdsclient.RepoResolver

	accounts AccountSource

	sessions SessionSource

	plc PLCClient

	relay RelaySource

	vault VaultSource

	log *slog.Logger

	retry time.Duration

	resync time.Duration
}

type reconciler func(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured)

func New(opts Options) (*Controller, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, fmt.Errorf("statusstate: a workspace is required")
	}
	if opts.Dynamic == nil {
		return nil, fmt.Errorf("statusstate: a dynamic client factory is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	retry := opts.RetryAfter
	if retry <= 0 {
		retry = DefaultRetryAfter
	}
	resync := opts.Resync
	if resync <= 0 {
		resync = DefaultResync
	}
	plcClient := opts.PLC
	if plcClient == nil && strings.TrimSpace(opts.PLCBaseURL) != "" {
		built, err := NewHTTPPLCClient(opts.PLCBaseURL, opts.PLCHandlers)
		if err != nil {
			return nil, err
		}
		plcClient = built
	}
	relaySource := opts.Relay
	if relaySource == nil && (opts.RelayEndpoint != nil || opts.RelayHandlers != nil) {
		relaySource = NewHTTPRelayHealthSource(opts.RelayEndpoint, opts.RelayHandlers)
	}
	accounts := opts.Accounts
	sessions := opts.Sessions
	if opts.RepoResolver != nil {
		if accounts == nil {
			accounts = RepoAccountSource{Resolver: opts.RepoResolver, Workspace: opts.Workspace}
		}
		if sessions == nil {
			sessions = RepoSessionSource{Resolver: opts.RepoResolver, Workspace: opts.Workspace}
		}
	}
	return &Controller{
		workspace: opts.Workspace,
		dynamic:   opts.Dynamic,
		pds:       opts.PDS,
		repo:      opts.RepoResolver,
		accounts:  accounts,
		sessions:  sessions,
		plc:       plcClient,
		relay:     relaySource,
		vault:     opts.Vault,
		log:       log.With("component", "statusstate"),
		retry:     retry,
		resync:    resync,
	}, nil
}

func Register(ctx context.Context, opts Options) error {
	ctl, err := New(opts)
	if err != nil {
		return err
	}
	return ctl.Run(ctx)
}

func (c *Controller) Run(ctx context.Context) error {
	kinds := []struct {
		gvr       schema.GroupVersionResource
		reconcile reconciler
	}{
		{PdsAccountGVR, c.reconcileAccount},
		{AtprotoSessionGVR, c.reconcileSession},
		{PlcIdentityGVR, c.reconcilePLC},
		{RelayStatusGVR, c.reconcileRelay},
		{VaultCredentialGVR, c.reconcileVaultCredential},
		{VaultCertificateGVR, c.reconcileVaultCertificate},
	}
	var wg sync.WaitGroup
	for _, kind := range kinds {
		wg.Add(1)
		go func(gvr schema.GroupVersionResource, reconcile reconciler) {
			defer wg.Done()
			c.runKind(ctx, gvr, reconcile)
		}(kind.gvr, kind.reconcile)
	}
	wg.Wait()
	return nil
}

func (c *Controller) runKind(ctx context.Context, gvr schema.GroupVersionResource, reconcile reconciler) {
	for ctx.Err() == nil {
		if err := c.watchKind(ctx, gvr, reconcile); err != nil && ctx.Err() == nil {
			c.log.Warn("watching status objects", "workspace", c.workspace, "resource", gvr.Resource, "err", err.Error())
		}
		if !sleep(ctx, c.retry) {
			return
		}
	}
}

func (c *Controller) watchKind(ctx context.Context, gvr schema.GroupVersionResource, reconcile reconciler) error {
	dyn, err := c.dynamic(c.workspace)
	if err != nil {
		return err
	}
	ri := dyn.Resource(gvr)
	list, err := ri.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		reconcile(ctx, dyn, &list.Items[i])
	}
	w, err := ri.Watch(ctx, metav1.ListOptions{ResourceVersion: list.GetResourceVersion()})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer w.Stop()
	timer := time.NewTimer(c.resync)
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
			if event.Type == watch.Deleted || event.Type == watch.Bookmark || event.Type == watch.Error {
				continue
			}
			u, ok := event.Object.(*unstructured.Unstructured)
			if !ok || u == nil || u.GetDeletionTimestamp() != nil {
				continue
			}
			reconcile(ctx, dyn, u)
		}
	}
}

func (c *Controller) write(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, u *unstructured.Unstructured, status any) {
	next, err := runtime.DefaultUnstructuredConverter.ToUnstructured(status)
	if err != nil {
		c.log.Warn("encoding a status", "workspace", c.workspace, "resource", gvr.Resource, "name", u.GetName(), "err", err.Error())
		return
	}
	ri := dyn.Resource(gvr)
	for range 4 {
		cur, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			c.log.Warn("reading an object for its status", "workspace", c.workspace, "resource", gvr.Resource, "name", u.GetName(), "err", err.Error())
			return
		}
		if reflect.DeepEqual(cur.Object["status"], next) {
			return
		}
		cur.Object["status"] = next
		if _, err := ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			c.log.Warn("writing a status", "workspace", c.workspace, "resource", gvr.Resource, "name", u.GetName(), "err", err.Error())
			return
		}
		return
	}
}

func decode(u *unstructured.Unstructured, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

func setReady(conditions *[]metav1.Condition, generation int64, ok bool, reason, message string) {
	value := metav1.ConditionFalse
	if ok {
		value = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               ConditionReady,
		Status:             value,
		Reason:             reason,
		Message:            truncate(message),
		ObservedGeneration: generation,
	})
}

func truncate(message string) string {
	if len(message) <= maxMessage {
		return message
	}
	return message[:maxMessage]
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

func refString(r commonv1.ObjectRef) string {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return ""
	}
	if kind := strings.TrimSpace(r.Kind); kind != "" {
		return kind + "/" + name
	}
	return name
}

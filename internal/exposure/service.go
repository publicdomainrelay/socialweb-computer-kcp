package exposure

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type ServiceOptions struct {
	Workspace  string
	Dynamic    DynamicFactory
	Handlers   *component.Handlers
	Registry   *component.Registry
	Log        *slog.Logger
	RetryAfter time.Duration
}

type ServiceController struct {
	workspace string
	dynamic   DynamicFactory
	handlers  *component.Handlers
	registry  *component.Registry
	log       *slog.Logger
	retry     time.Duration
}

func NewServiceController(opts ServiceOptions) (*ServiceController, error) {
	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		return nil, errors.New("exposure: a workspace is required for the Service controller")
	}
	if opts.Dynamic == nil {
		return nil, errors.New("exposure: a dynamic client factory is required for the Service controller")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if opts.RetryAfter <= 0 {
		opts.RetryAfter = DefaultRetryAfter
	}
	return &ServiceController{
		workspace: workspace,
		dynamic:   opts.Dynamic,
		handlers:  opts.Handlers,
		registry:  opts.Registry,
		log:       log.With("component", "service-controller"),
		retry:     opts.RetryAfter,
	}, nil
}

func (c *ServiceController) Run(ctx context.Context) error {
	w := &watcher{
		workspace: c.workspace,
		dynamic:   c.dynamic,
		gvr:       ServiceGVR,
		log:       c.log,
		retry:     c.retry,
		reconcile: c.Reconcile,
	}
	return w.Run(ctx)
}

func (c *ServiceController) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if u == nil {
		return nil
	}
	ri := dyn.Resource(ServiceGVR)
	name := u.GetName()
	if u.GetDeletionTimestamp() != nil {
		return ensureMetadata(ctx, ri, name, ServiceFinalizer, false, nil)
	}
	if err := ensureMetadata(ctx, ri, name, ServiceFinalizer, true, nil); err != nil {
		return err
	}
	var svc exposurev1alpha1.Service
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &svc); err != nil {
		return fmt.Errorf("exposure: decoding Service %s: %w", name, err)
	}
	generation := u.GetGeneration()
	status := svc.Status
	status.Conditions = append([]metav1.Condition(nil), status.Conditions...)
	if svc.Spec.Suspend {
		status.Phase = exposurev1alpha1.ServicePending
		status.Ready = false
		status.Endpoints = nil
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "Suspended", "the Service is suspended")
		setCondition(&status.Conditions, generation, ConditionReady, false, "Suspended", "the Service is suspended")
		return writeStatus(ctx, ri, name, &status)
	}
	target, err := targetKey(svc.Spec.TargetRef, c.workspace)
	if err != nil {
		status.Phase = exposurev1alpha1.ServiceFailed
		status.Ready = false
		status.Endpoints = nil
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidTarget", err.Error())
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidTarget", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	_, found, err := checkHandler(ctx, c.handlers, target, svc.Spec.Protocol, svc.Spec.Handler)
	if err != nil {
		status.Phase = exposurev1alpha1.ServiceFailed
		status.Ready = false
		status.Endpoints = nil
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "InvalidProtocol", err.Error())
		setCondition(&status.Conditions, generation, ConditionReady, false, "InvalidProtocol", err.Error())
		return writeStatus(ctx, ri, name, &status)
	}
	if !found {
		status.Phase = exposurev1alpha1.ServicePending
		status.Ready = false
		status.Endpoints = nil
		message := fmt.Sprintf("no handler %q is registered for %s/%s in workspace %s", handlerName(svc.Spec.Handler), target.Kind, target.Name, target.Workspace)
		setCondition(&status.Conditions, generation, ConditionHandlerRegistered, false, "HandlerMissing", message)
		setCondition(&status.Conditions, generation, ConditionReady, false, "HandlerMissing", message)
		return writeStatus(ctx, ri, name, &status)
	}
	status.Phase = exposurev1alpha1.ServiceReady
	status.Ready = true
	status.Endpoints = endpointsFor(c.registry, target)
	message := fmt.Sprintf("the handler %q is registered for %s/%s", handlerName(svc.Spec.Handler), target.Kind, target.Name)
	setCondition(&status.Conditions, generation, ConditionHandlerRegistered, true, "HandlerRegistered", message)
	setCondition(&status.Conditions, generation, ConditionReady, true, "Ready", message)
	return writeStatus(ctx, ri, name, &status)
}

func targetKey(ref commonv1.ObjectRef, workspace string) (component.Key, error) {
	kind := strings.TrimSpace(ref.Kind)
	name := strings.TrimSpace(ref.Name)
	if kind == "" {
		return component.Key{}, errors.New("the targetRef has no kind")
	}
	if name == "" {
		return component.Key{}, errors.New("the targetRef has no name")
	}
	targetWorkspace := strings.TrimSpace(ref.Workspace)
	if targetWorkspace == "" {
		targetWorkspace = workspace
	}
	return component.Key{Workspace: targetWorkspace, Kind: kind, Name: name}, nil
}

func protocolKind(value exposurev1alpha1.ServiceProtocol) string {
	return strings.ToLower(strings.TrimSpace(string(value)))
}

func handlerName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return component.DefaultHandlerName
	}
	return value
}

func checkHandler(ctx context.Context, handlers *component.Handlers, key component.Key, protocol exposurev1alpha1.ServiceProtocol, name string) (http.Handler, bool, error) {
	if handlers == nil {
		return nil, false, nil
	}
	switch protocolKind(protocol) {
	case component.ProtocolHTTP, component.ProtocolWS:
		handler, ok := handlers.Handler(key, name)
		return handler, ok, nil
	case component.ProtocolTCP, component.ProtocolSSH:
		conn, err := handlers.DialStream(ctx, key, name)
		if err != nil {
			if errors.Is(err, component.ErrNoStream) || errors.Is(err, component.ErrHandlersStopped) {
				return nil, false, nil
			}
			return nil, false, err
		}
		_ = conn.Close()
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("unknown protocol %q", protocol)
	}
}

func endpointsFor(registry *component.Registry, key component.Key) []string {
	if registry == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, endpoint := range registry.Endpoints(key.Workspace, key.Kind, key.Name) {
		value := strings.TrimSpace(endpoint.URL)
		if value == "" && endpoint.Address != "" {
			protocol := strings.TrimSpace(endpoint.Protocol)
			if protocol != "" {
				value = protocol + "://" + endpoint.Address
			} else {
				value = endpoint.Address
			}
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

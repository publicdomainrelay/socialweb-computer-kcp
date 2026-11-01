package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/oauthcallback"
	servicesrequester "github.com/johnandersen777/socialweb-computer-kcp/internal/services/requester"
)

const runnerFieldManager = "socialweb.computer/embedded-requester"

type requesterRunner struct {
	runtime *component.Runtime
}

func registerRequesterRunner(ctx context.Context, m *ctlruntime.Manager, b *bus.Bus, callbacks *oauthcallback.Registry,
	stateDir string, scope func(string) bool, log *slog.Logger) error {
	rt := component.NewRuntime(component.RuntimeConfig{Log: log, StateDir: stateDir, Registry: component.NewRegistry(), Bus: b}, nil)
	factory := component.Typed(func(ctx context.Context, in component.Instance, spec v1alpha1.RequesterSpec) (*servicesrequester.Service, error) {
		return servicesrequester.New(ctx, spec, servicesrequester.Deps{
			Instance:       in,
			Egress:         component.Egress{Log: log},
			OAuthCallbacks: callbacks,
		})
	})
	if err := rt.Register(servicesrequester.Kind, factory); err != nil {
		return err
	}
	if err := rt.Start(ctx); err != nil {
		return err
	}
	options := []ctlruntime.Option{
		ctlruntime.Export(compose.ExportApplications),
		ctlruntime.FieldManager(runnerFieldManager),
		ctlruntime.SharedStatus(),
		ctlruntime.InWorkspaces(scope),
	}
	if err := ctlruntime.For[*v1alpha1.Requester](m, "embedded-requester", &requesterRunner{runtime: rt}, options...); err != nil {
		return fmt.Errorf("embedded: the requester runner: %w", err)
	}
	return nil
}

func (r *requesterRunner) Reconcile(ctx context.Context, req ctlruntime.Request, obj *v1alpha1.Requester) (ctlruntime.Result, error) {
	raw, err := json.Marshal(obj.Spec)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	status, err := r.runtime.Reconcile(ctx, component.ComponentSpec{
		UID:         types.UID(obj.UID),
		Group:       v1alpha1.GroupName,
		Kind:        servicesrequester.Kind,
		Name:        obj.Name,
		Workspace:   req.Workspace,
		Generation:  obj.Generation,
		Suspend:     obj.Spec.Suspend,
		Deleting:    !obj.DeletionTimestamp.IsZero(),
		StateDir:    obj.Spec.StateDir,
		Spec:        raw,
		Annotations: obj.Annotations,
		Labels:      obj.Labels,
	})
	next := obj.DeepCopy()
	next.Status = status.APIStatus()
	if applyErr := req.ApplyStatus(ctx, next); applyErr != nil {
		return ctlruntime.Result{}, applyErr
	}
	return ctlruntime.Result{}, err
}

func (r *requesterRunner) Remove(ctx context.Context, uid types.UID) error {
	return r.runtime.Remove(ctx, uid)
}

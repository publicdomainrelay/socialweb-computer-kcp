package componentcore

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	guestcontrolplanectl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/guestcontrolplane"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

type guestControlPlaneStore struct {
	clients *workspaceClients
}

var _ guestcontrolplanectl.Store = guestControlPlaneStore{}

func (c *Core) registerGuestControlPlane(mgr *ctlruntime.Manager, scope func(string) bool) error {
	restCfg, err := c.managerRestConfig()
	if err != nil {
		return err
	}
	clients, err := newWorkspaceClients(restCfg)
	if err != nil {
		return err
	}
	_, err = guestcontrolplanectl.Register(mgr, guestcontrolplanectl.Options{Bus: c.handlers, Store: guestControlPlaneStore{clients: clients},
		Scope: scope, Log: c.cfg.Log})
	return err
}

func (s guestControlPlaneStore) Get(ctx context.Context, workspace, name string, obj client.Object) error {
	c, err := s.clients.client(workspace)
	if err != nil {
		return err
	}
	return c.Get(ctx, client.ObjectKey{Name: name}, obj)
}

func (s guestControlPlaneStore) List(ctx context.Context, workspace string, list client.ObjectList) error {
	return s.clients.List(ctx, workspace, list)
}

func (s guestControlPlaneStore) Apply(ctx context.Context, workspace, fieldManager string, obj kube.Doc, status bool) error {
	c, err := s.clients.client(workspace)
	if err != nil {
		return err
	}
	if err := kube.ApplyWith(ctx, c, obj, kube.ApplyOptions{FieldManager: fieldManager, Force: true, Status: status}); err != nil {
		meta, _ := obj["metadata"].(map[string]any)
		kind, _ := obj["kind"].(string)
		name, _ := meta["name"].(string)
		return fmt.Errorf("componentcore: applying %s %s in %s: %w", kind, name, workspace, err)
	}
	return nil
}

func (s guestControlPlaneStore) Delete(ctx context.Context, workspace string, obj client.Object) error {
	c, err := s.clients.client(workspace)
	if err != nil {
		return err
	}
	if err := c.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

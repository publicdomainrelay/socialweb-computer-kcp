package appctl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	AppWorkspaceLabel = "apps.socialweb.computer/application"
	AppWorkspaceType  = "universal"
)

var reservedAppNames = []string{"global", "relay", "alice", "bob", "dev", ComponentWorkspace}

func checkAppName(name string) error {
	if slices.Contains(reservedAppNames, name) || strings.HasSuffix(name, manifests.SharedWorkspaceSuffix) {
		return fmt.Errorf("apps[].name %q names a workspace the market network keeps for itself (%s, *%s)", name,
			strings.Join(reservedAppNames, ", "), manifests.SharedWorkspaceSuffix)
	}
	return nil
}

func (n *networks) appWorkspace(name string) string { return n.ctl.c.workspace() + ":" + name }

func (n *networks) ensureAppWorkspace(ctx context.Context, name string) error {
	ri, err := n.ctl.client(n.ctl.c.workspace(), WorkspaceGVR)
	if err != nil {
		return err
	}
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "tenancy.kcp.io/v1alpha1", "kind": "Workspace",
		"metadata": map[string]any{"name": name, "labels": map[string]any{AppWorkspaceLabel: name},
			"annotations": map[string]any{kcpserver.WorkspaceOwnerAnnotation: kcpserver.ShardAdminOwner}},
		"spec": map[string]any{"type": map[string]any{"name": AppWorkspaceType, "path": DefaultWorkspace}},
	}}
	if _, err := ri.Create(ctx, o, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating the workspace %s: %w", n.appWorkspace(name), err)
	}
	deadline := time.Now().Add(ApplyTimeout)
	for {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		phase := ""
		if err == nil {
			if cur.GetDeletionTimestamp() != nil {
				return fmt.Errorf("the workspace %s is being deleted; the next pass places it again", n.appWorkspace(name))
			}
			phase, _, _ = unstructured.NestedString(cur.Object, "status", "phase")
		}
		if phase == "Ready" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the workspace %s is not Ready within %s (phase %q): %v", n.appWorkspace(name), ApplyTimeout, phase, err)
		}
		if !sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (n *networks) deleteAppWorkspace(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	ri, err := n.ctl.client(n.ctl.c.workspace(), WorkspaceGVR)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		if _, err := ri.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the workspace %s is still there %s after its delete", n.appWorkspace(name), ChildDeleteTimeout)
		}
		if !sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (n *networks) stopAppPods(ctx context.Context, name string) error {
	ws := n.appWorkspace(name) + ":" + ComponentWorkspace
	ri, err := n.ctl.client(ws, DenoPodGVR)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		list, err := ri.Namespace("default").List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) || (err == nil && len(list.Items) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, p := range list.Items {
			if p.GetDeletionTimestamp() == nil {
				if err := ri.Namespace("default").Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d DenoPod(s) of %s still run %s after their delete", len(list.Items), ws, ChildDeleteTimeout)
		}
		if !sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

package marketnetwork

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/manifests"
)

const (
	AppWorkspaceLabel = "apps.socialweb.computer/application"
	AppWorkspaceType  = "universal"
	PeerReadRole      = "market-network-peers"

	accountWorkspaceType = "account"
	foreignAccountPrefix = "system:kcp:serviceaccount:"
)

var (
	clusterRoleGVR        = schema.GroupVersionResource{Group: rbacv1.GroupName, Version: "v1", Resource: "clusterroles"}
	clusterRoleBindingGVR = schema.GroupVersionResource{Group: rbacv1.GroupName, Version: "v1", Resource: "clusterrolebindings"}
	serviceAccountGVR     = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
)

var reservedAppNames = []string{"global", "relay", "alice", "bob", "dev", compose.ComponentWorkspace}

func ForeignAccount(cluster, namespace, name string) string {
	return foreignAccountPrefix + cluster + ":" + namespace + ":" + name
}

func checkAppName(name string) error {
	if slices.Contains(reservedAppNames, name) || strings.HasSuffix(name, manifests.SharedWorkspaceSuffix) {
		return fmt.Errorf("apps[].name %q names a workspace the market network keeps for itself (%s, *%s)", name,
			strings.Join(reservedAppNames, ", "), manifests.SharedWorkspaceSuffix)
	}
	return nil
}

func (ctl *Controller) appWorkspace(name string) string { return ctl.c.Workspace() + ":" + name }

func (ctl *Controller) applyRBAC(ctx context.Context, r compose.Root, ws string, gvr schema.GroupVersionResource, obj any, kind string) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(gvr.GroupVersion().String())
	u.SetKind(kind)
	labels := r.Labels(kind)
	for k, v := range u.GetLabels() {
		labels[k] = v
	}
	u.SetLabels(labels)
	return ctl.c.Apply(ctx, r, ws, gvr, u)
}

func (ctl *Controller) ensureAppWorkspace(ctx context.Context, r compose.Root, name string) error {
	ws := ctl.c.Workspace()
	u := ctl.c.Object(r, ws, "tenancy.kcp.io/v1alpha1", "Workspace", name, map[string]string{AppWorkspaceLabel: name})
	u.SetAnnotations(map[string]string{kcpserver.WorkspaceOwnerAnnotation: kcpserver.ShardAdminOwner})
	u.Object["spec"] = map[string]any{"type": map[string]any{"name": AppWorkspaceType, "path": compose.DefaultWorkspace}}
	if err := ctl.c.Apply(ctx, r, ws, compose.WorkspaceGVR, u); err != nil {
		return fmt.Errorf("applying the workspace %s: %w", ctl.appWorkspace(name), err)
	}
	ri, err := ctl.c.Client(ws, compose.WorkspaceGVR)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(compose.ApplyTimeout)
	for {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		phase := ""
		if err == nil {
			if cur.GetDeletionTimestamp() != nil {
				return fmt.Errorf("the workspace %s is being deleted; the next reconcile places it again", ctl.appWorkspace(name))
			}
			phase, _, _ = unstructured.NestedString(cur.Object, "status", "phase")
		}
		if phase == "Ready" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the workspace %s is not Ready within %s (phase %q): %v", ctl.appWorkspace(name), compose.ApplyTimeout, phase, err)
		}
		if !clock.Sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (ctl *Controller) waitGone(ctx context.Context, ws string, gvr schema.GroupVersionResource, name, what string) error {
	ri, err := ctl.c.Client(ws, gvr)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		if _, err := ri.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s is still there %s after its delete", what, ChildDeleteTimeout)
		}
		if !clock.Sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (ctl *Controller) deleteAppWorkspace(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	ri, err := ctl.c.Client(ctl.c.Workspace(), compose.WorkspaceGVR)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return ctl.waitGone(ctx, ctl.c.Workspace(), compose.WorkspaceGVR, name, "the workspace "+ctl.appWorkspace(name))
}

func (ctl *Controller) deleteAccountWorkspaces(ctx context.Context) error {
	ri, err := ctl.c.Client(ctl.c.Workspace(), compose.WorkspaceGVR)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		list, err := ri.List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		left := 0
		for _, w := range list.Items {
			if t, _, _ := unstructured.NestedString(w.Object, "spec", "type", "name"); t != accountWorkspaceType {
				continue
			}
			left++
			if w.GetDeletionTimestamp() == nil {
				if err := ri.Delete(ctx, w.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d account workspace(s) are still there %s after their delete", left, ChildDeleteTimeout)
		}
		if !clock.Sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (ctl *Controller) stopAppComponents(ctx context.Context, name string) error {
	ws := ctl.appWorkspace(name) + ":" + compose.ComponentWorkspace
	types := append(append([]compose.ComponentType{}, compose.ComponentTypes...), compose.PolicyTypes...)
	deadline := time.Now().Add(ChildDeleteTimeout)
	for {
		left := 0
		seen := map[string]bool{}
		for _, ct := range types {
			if seen[ct.Kind] {
				continue
			}
			seen[ct.Kind] = true
			ri, err := ctl.c.Client(ws, ct.GVR)
			if err != nil {
				return err
			}
			list, err := ri.Namespace("default").List(ctx, metav1.ListOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			left += len(list.Items)
			for _, p := range list.Items {
				if p.GetDeletionTimestamp() != nil {
					continue
				}
				if err := ri.Namespace("default").Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
			}
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d component(s) of %s still run %s after their delete", left, ws, ChildDeleteTimeout)
		}
		if !clock.Sleep(ctx, ChildPoll) {
			return ctx.Err()
		}
	}
}

func (ctl *Controller) clusterOf(ctx context.Context, parent, name string) string {
	u, err := ctl.c.Get(ctx, parent, compose.WorkspaceGVR, name)
	if err != nil {
		return ""
	}
	id, _, _ := unstructured.NestedString(u.Object, "spec", "cluster")
	return id
}

func (ctl *Controller) grantPeers(ctx context.Context, r compose.Root, names []string) error {
	ids := map[string]map[string]string{}
	for _, name := range names {
		ws := ctl.appWorkspace(name)
		own := map[string]string{}
		if id := ctl.clusterOf(ctx, ctl.c.Workspace(), name); id != "" {
			own[ws] = id
		}
		if id := ctl.clusterOf(ctx, ws, compose.ComponentWorkspace); id != "" {
			own[ws+":"+compose.ComponentWorkspace] = id
		}
		if len(own) > 0 {
			ids[name] = own
		}
	}
	key := fmt.Sprint(ids)
	if ctl.peers == key {
		return nil
	}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: PeerReadRole}, Rules: []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/"}, Verbs: []string{"access"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"marketidentities"}, Verbs: []string{"get", "list"}}}}
	for name, own := range ids {
		var readers []rbacv1.Subject
		for other, theirs := range ids {
			if other == name {
				continue
			}
			for _, id := range theirs {
				readers = append(readers, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: ForeignAccount(id, "default", compose.PdsComponent)})
			}
		}
		slices.SortFunc(readers, func(a, b rbacv1.Subject) int { return strings.Compare(a.Name, b.Name) })
		for ws := range own {
			if err := ctl.applyRBAC(ctx, r, ws, clusterRoleGVR, role, "ClusterRole"); err != nil {
				return fmt.Errorf("the peer read role in %s: %w", ws, err)
			}
			b := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: PeerReadRole}, Subjects: readers,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: PeerReadRole}}
			if err := ctl.applyRBAC(ctx, r, ws, clusterRoleBindingGVR, b, "ClusterRoleBinding"); err != nil {
				return fmt.Errorf("the peer read binding in %s: %w", ws, err)
			}
		}
	}
	ctl.peers = key
	ctl.log().Info("the pds account of every child application may read the MarketIdentity objects of the others", "clusters", key)
	return nil
}

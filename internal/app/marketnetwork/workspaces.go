package marketnetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	rbacv1 "k8s.io/api/rbac/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
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
	last, err := ctlruntime.Await(ctx, ri, name, compose.ApplyTimeout, func(cur *unstructured.Unstructured) (bool, error) {
		if cur == nil {
			return false, nil
		}
		if cur.GetDeletionTimestamp() != nil {
			return false, fmt.Errorf("the workspace %s is being deleted; the next reconcile places it again", ctl.appWorkspace(name))
		}
		phase, _, _ := unstructured.NestedString(cur.Object, "status", "phase")
		return phase == "Ready", nil
	})
	if !errors.Is(err, ctlruntime.ErrAwaitTimeout) {
		return err
	}
	phase := ""
	var getErr error
	if last == nil {
		getErr = apierrors.NewNotFound(compose.WorkspaceGVR.GroupResource(), name)
	} else {
		phase, _, _ = unstructured.NestedString(last.Object, "status", "phase")
	}
	return fmt.Errorf("the workspace %s is not Ready within %s (phase %q): %v", ctl.appWorkspace(name), compose.ApplyTimeout, phase, getErr)
}

func (ctl *Controller) waitGone(ctx context.Context, ws string, gvr schema.GroupVersionResource, name, what string) error {
	ri, err := ctl.c.Client(ws, gvr)
	if err != nil {
		return err
	}
	_, err = ctlruntime.Await(ctx, ri, name, ChildDeleteTimeout, ctlruntime.Absent)
	if errors.Is(err, ctlruntime.ErrAwaitTimeout) {
		return fmt.Errorf("%s is still there %s after its delete", what, ChildDeleteTimeout)
	}
	return err
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
	deleted := map[string]bool{}
	left := 0
	_, err = ctlruntime.AwaitList(ctx, ri, metav1.ListOptions{}, ChildDeleteTimeout, func(items []*unstructured.Unstructured) (bool, error) {
		left = 0
		for _, w := range items {
			if t, _, _ := unstructured.NestedString(w.Object, "spec", "type", "name"); t != accountWorkspaceType {
				continue
			}
			left++
			if w.GetDeletionTimestamp() == nil && !deleted[w.GetName()] {
				if err := ri.Delete(ctx, w.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return false, err
				}
				deleted[w.GetName()] = true
			}
		}
		return left == 0, nil
	})
	if errors.Is(err, ctlruntime.ErrAwaitTimeout) {
		return fmt.Errorf("%d account workspace(s) are still there %s after their delete", left, ChildDeleteTimeout)
	}
	return err
}

func (ctl *Controller) stopAppComponents(ctx context.Context, name string) error {
	ws := ctl.appWorkspace(name) + ":" + compose.ComponentWorkspace
	types := append(append([]compose.ComponentType{}, compose.ComponentTypes...), compose.PolicyTypes...)
	deadline := time.Now().Add(ChildDeleteTimeout)
	var pending []dynamic.ResourceInterface
	seen := map[string]bool{}
	for _, ct := range types {
		if seen[ct.Kind] {
			continue
		}
		seen[ct.Kind] = true
		nri, err := ctl.c.Client(ws, ct.GVR)
		if err != nil {
			return err
		}
		ri := nri.Namespace("default")
		list, err := ri.List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if len(list.Items) == 0 {
			continue
		}
		if err := deleteListed(ctx, ri, list.Items, nil); err != nil {
			return err
		}
		pending = append(pending, ri)
	}
	left := 0
	for _, ri := range pending {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			list, err := ri.List(ctx, metav1.ListOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil {
				left += len(list.Items)
			}
			continue
		}
		deleted := map[string]bool{}
		items, err := ctlruntime.AwaitList(ctx, ri, metav1.ListOptions{}, remaining, func(items []*unstructured.Unstructured) (bool, error) {
			listed := make([]unstructured.Unstructured, 0, len(items))
			for _, it := range items {
				listed = append(listed, *it)
			}
			return len(items) == 0, deleteListed(ctx, ri, listed, deleted)
		})
		if errors.Is(err, ctlruntime.ErrAwaitTimeout) {
			left += len(items)
			continue
		}
		if err != nil {
			return err
		}
	}
	if left > 0 {
		return fmt.Errorf("%d component(s) of %s still run %s after their delete", left, ws, ChildDeleteTimeout)
	}
	return nil
}

func deleteListed(ctx context.Context, ri dynamic.ResourceInterface, items []unstructured.Unstructured, deleted map[string]bool) error {
	for _, p := range items {
		if p.GetDeletionTimestamp() != nil || deleted[p.GetName()] {
			continue
		}
		if err := ri.Delete(ctx, p.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if deleted != nil {
			deleted[p.GetName()] = true
		}
	}
	return nil
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

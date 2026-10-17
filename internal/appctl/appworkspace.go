package appctl

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
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

const PeerReadRole = "market-network-peers"

func (n *networks) clusterOf(ctx context.Context, parent, name string) string {
	ri, err := n.ctl.client(parent, WorkspaceGVR)
	if err != nil {
		return ""
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	id, _, _ := unstructured.NestedString(u.Object, "spec", "cluster")
	return id
}

func (n *networks) grantPeers(ctx context.Context, names []string) error {
	ids := map[string]map[string]string{}
	for _, name := range names {
		ws := n.appWorkspace(name)
		own := map[string]string{}
		if id := n.clusterOf(ctx, n.ctl.c.workspace(), name); id != "" {
			own[ws] = id
		}
		if id := n.clusterOf(ctx, ws, ComponentWorkspace); id != "" {
			own[ws+":"+ComponentWorkspace] = id
		}
		if len(own) > 0 {
			ids[name] = own
		}
	}
	key := fmt.Sprint(ids)
	if n.peers == key {
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
				readers = append(readers, rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: ForeignAccount(id, "default", PdsComponent)})
			}
		}
		slices.SortFunc(readers, func(a, b rbacv1.Subject) int { return strings.Compare(a.Name, b.Name) })
		for ws := range own {
			cs, err := kubernetes.NewForConfig(clusterConfig(n.ctl.restCfg, ws))
			if err != nil {
				return err
			}
			if err := upsertClusterRole(ctx, cs, role); err != nil {
				return fmt.Errorf("the peer read role in %s: %w", ws, err)
			}
			b := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: PeerReadRole}, Subjects: readers,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: PeerReadRole}}
			if err := upsertClusterRoleBinding(ctx, cs, b); err != nil {
				return fmt.Errorf("the peer read binding in %s: %w", ws, err)
			}
		}
	}
	n.peers = key
	n.ctl.c.log().Info("the pds account of every child application may read the MarketIdentity objects of the others", "clusters", key)
	return nil
}

func upsertClusterRole(ctx context.Context, cs kubernetes.Interface, want *rbacv1.ClusterRole) error {
	cur, err := cs.RbacV1().ClusterRoles().Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.RbacV1().ClusterRoles().Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager})
		return err
	}
	if err != nil {
		return err
	}
	cur.Rules = want.Rules
	_, err = cs.RbacV1().ClusterRoles().Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
	return err
}

func upsertClusterRoleBinding(ctx context.Context, cs kubernetes.Interface, want *rbacv1.ClusterRoleBinding) error {
	cur, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, want.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = cs.RbacV1().ClusterRoleBindings().Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager})
		return err
	}
	if err != nil {
		return err
	}
	cur.Subjects = want.Subjects
	_, err = cs.RbacV1().ClusterRoleBindings().Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
	return err
}

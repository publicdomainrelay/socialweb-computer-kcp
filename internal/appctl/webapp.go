package appctl

import (
	"context"
	"fmt"
	"net/url"
	"os"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultlayout"
)

const (
	WebAppAccount        = "socialweb-computer-k8s"
	WebAppAccountNS      = "default"
	WebAppGrant          = "socialweb-computer-k8s-web-app"
	RequesterGrant       = "socialweb-computer-k8s-requester"
	foreignAccountPrefix = "system:kcp:serviceaccount:"
)

var WorkspaceGVR = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}

func WebAppAccountRef(workspace string) string {
	return workspace + ":" + ComponentWorkspace + "/" + WebAppAccountNS + "/" + WebAppAccount
}

func ForeignAccount(cluster, namespace, name string) string {
	return foreignAccountPrefix + cluster + ":" + namespace + ":" + name
}

func WebAppRootRules(gateway string) []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/"}, Verbs: []string{"access"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"gateways"}, ResourceNames: []string{gateway}, Verbs: []string{"get"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"openbaoservers"}, ResourceNames: []string{vaultlayout.RootServerName}, Verbs: []string{"get"}},
	}
}

type front struct {
	server     string
	ca         []byte
	serverName string
}

func readFront(kubeconfig string) (front, error) {
	cfg, err := clientcmd.LoadFromFile(kubeconfig)
	if err != nil {
		return front{}, err
	}
	kctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return front{}, fmt.Errorf("appctl: %s has no current context", kubeconfig)
	}
	c, ok := cfg.Clusters[kctx.Cluster]
	if !ok {
		return front{}, fmt.Errorf("appctl: %s names no cluster %s", kubeconfig, kctx.Cluster)
	}
	u, err := url.Parse(c.Server)
	if err != nil {
		return front{}, err
	}
	ca := c.CertificateAuthorityData
	if len(ca) == 0 && c.CertificateAuthority != "" {
		if ca, err = os.ReadFile(c.CertificateAuthority); err != nil {
			return front{}, err
		}
	}
	return front{server: u.Scheme + "://" + u.Host, ca: ca, serverName: c.TLSServerName}, nil
}

func (ctl *Controller) logicalCluster(ctx context.Context, parent, name string) (string, error) {
	ri, err := ctl.client(parent, WorkspaceGVR)
	if err != nil {
		return "", err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("appctl: the workspace %s:%s: %w", parent, name, err)
	}
	cluster, _, _ := unstructured.NestedString(u.Object, "spec", "cluster")
	if cluster == "" {
		return "", fmt.Errorf("appctl: the workspace %s:%s has no logical cluster yet", parent, name)
	}
	return cluster, nil
}

func RequesterRootRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/"}, Verbs: []string{"access"}},
		{APIGroups: []string{v1alpha1.GroupName}, Resources: []string{"openbaoservers"}, ResourceNames: []string{vaultlayout.RootServerName}, Verbs: []string{"get"}},
	}
}

func (ctl *Controller) grant(ctx context.Context, ws, owner, grantName, user string, rules []rbacv1.PolicyRule) error {
	cs, err := kubernetes.NewForConfig(clusterConfig(ctl.restCfg, ws))
	if err != nil {
		return err
	}
	labels := map[string]string{OwnerLabel: owner}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: grantName, Labels: labels}, Rules: rules}
	have, err := cs.RbacV1().ClusterRoles().Get(ctx, grantName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := cs.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
			return fmt.Errorf("appctl: the %s ClusterRole in %s: %w", grantName, ws, err)
		}
	case err != nil:
		return err
	case !equality.Semantic.DeepEqual(have.Rules, rules):
		have.Rules, have.Labels = rules, labels
		if _, err := cs.RbacV1().ClusterRoles().Update(ctx, have, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
			return fmt.Errorf("appctl: the %s ClusterRole in %s: %w", grantName, ws, err)
		}
	}
	subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: user}}
	ref := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: grantName}
	binding := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: grantName, Labels: labels}, RoleRef: ref, Subjects: subjects}
	hb, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, grantName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := cs.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
			return fmt.Errorf("appctl: the %s ClusterRoleBinding in %s: %w", grantName, ws, err)
		}
	case err != nil:
		return err
	case !equality.Semantic.DeepEqual(hb.Subjects, subjects):
		hb.Subjects, hb.Labels = subjects, labels
		if _, err := cs.RbacV1().ClusterRoleBindings().Update(ctx, hb, metav1.UpdateOptions{FieldManager: FieldManager}); err != nil {
			return fmt.Errorf("appctl: the %s ClusterRoleBinding in %s: %w", grantName, ws, err)
		}
	}
	return nil
}

func (ctl *Controller) revokeWebApp(ctx context.Context) error {
	cs, err := kubernetes.NewForConfig(clusterConfig(ctl.restCfg, ctl.c.workspace()))
	if err != nil {
		return err
	}
	for _, name := range []string{WebAppGrant, RequesterGrant} {
		if err := cs.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err := cs.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (ctl *Controller) webAppIdentity(ctx context.Context, o v1alpha1.Gateway, owner string, w WebApp) (string, error) {
	f, err := readFront(w.FrontKubeconfig)
	if err != nil {
		return "", err
	}
	app, err := ctl.logicalCluster(ctx, ctl.c.workspace(), ComponentWorkspace)
	if err != nil {
		return "", err
	}
	user := ForeignAccount(app, WebAppAccountNS, WebAppAccount)
	if err := ctl.grant(ctx, ctl.c.workspace(), owner, WebAppGrant, user, WebAppRootRules(o.Name)); err != nil {
		return "", err
	}
	if err := ctl.grant(ctx, ctl.c.workspace(), owner, RequesterGrant, ForeignAccount(app, WebAppAccountNS, RequesterAccount), RequesterRootRules()); err != nil {
		return "", err
	}
	if err := ctl.grant(ctx, ctl.c.workspace(), owner, GatewayPdsGrant, ForeignAccount(app, WebAppAccountNS, PdsComponent), RequesterRootRules()); err != nil {
		return "", err
	}
	ctl.c.log().Info("the web app's own kcp identity", "user", user, "workspace", ctl.c.workspace()+":"+ComponentWorkspace, "server", f.server)
	return f.server, nil
}

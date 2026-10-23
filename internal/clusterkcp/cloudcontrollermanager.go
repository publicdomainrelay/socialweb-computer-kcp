package clusterkcp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
)

const cloudControllerManagerKubeconfigMode = 0o600

var _ clusterprovider.KubeconfigWriter = WriteKubeconfig

func WriteKubeconfig(path string, cfg *clusterprovider.Credential, contextName string, unwritable error) error {
	if cfg == nil {
		return fmt.Errorf("%w: no credential was rendered for %s", unwritable, path)
	}
	if path == "" {
		return fmt.Errorf("%w: the target path is empty", unwritable)
	}
	body, err := clientcmd.Write(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{contextName: {
			Server:                   cfg.Host,
			CertificateAuthorityData: cfg.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{contextName: {
			ClientCertificateData: cfg.CertData,
			ClientKeyData:         cfg.KeyData,
		}},
		Contexts: map[string]*clientcmdapi.Context{contextName: {
			Cluster:  contextName,
			AuthInfo: contextName,
		}},
		CurrentContext: contextName,
	})
	if err != nil {
		return fmt.Errorf("%w: rendering the credential for %s: %v", unwritable, path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%w: the directory %s holding %s: %v",
			unwritable, dir, path, err)
	}
	if info, err := os.Stat(path); err == nil {
		current, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: reading %s: %v", unwritable, path, err)
		}
		if string(current) == string(body) && info.Mode().Perm() == cloudControllerManagerKubeconfigMode {
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: stat %s: %v", unwritable, path, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".")
	if err != nil {
		return fmt.Errorf("%w: a temporary file beside %s: %v",
			unwritable, path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(cloudControllerManagerKubeconfigMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: the mode of %s: %v", unwritable, tmp.Name(), err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("%w: writing %s: %v", unwritable, tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("%w: closing %s: %v", unwritable, tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("%w: renaming %s over %s: %v",
			unwritable, tmp.Name(), path, err)
	}
	return nil
}

func GrantCloudControllerManager(ctx context.Context, core kubernetes.Interface, cluster string) error {
	if core == nil {
		return fmt.Errorf("%w (cluster %s)", clusterprovider.ErrNoCloudControllerManagerCredential, cluster)
	}
	role := cloudControllerManagerClusterRole()
	if _, err := core.RbacV1().ClusterRoles().Create(ctx, role, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("clusterprovider: creating the RBAC ClusterRole %s: %w", role.Name, err)
		}
		if _, err := core.RbacV1().ClusterRoles().Update(ctx, role, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("clusterprovider: updating the RBAC ClusterRole %s: %w", role.Name, err)
		}
	}
	binding := cloudControllerManagerClusterRoleBinding()
	if _, err := core.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{FieldManager: fieldManager}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("clusterprovider: creating the RBAC ClusterRoleBinding %s: %w", binding.Name, err)
		}
		if _, err := core.RbacV1().ClusterRoleBindings().Update(ctx, binding, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("clusterprovider: updating the RBAC ClusterRoleBinding %s: %w", binding.Name, err)
		}
	}
	return nil
}

func cloudControllerManagerClusterRole() *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: clusterprovider.CloudControllerManagerClusterRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"nodes"},
			Verbs:     []string{"get", "list", "watch", "patch", "update", "delete"},
		}},
	}
}

func cloudControllerManagerClusterRoleBinding() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterprovider.CloudControllerManagerClusterRoleBinding},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterprovider.CloudControllerManagerClusterRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:     rbacv1.GroupKind,
			APIGroup: rbacv1.GroupName,
			Name:     clusterprovider.CloudControllerManagerGroup,
		}},
	}
}

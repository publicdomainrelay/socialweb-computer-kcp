package clusterkcp

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
)

type IdentityServiceAccounts struct{}

var _ clusterprovider.ServiceAccounts = IdentityServiceAccounts{}

func (IdentityServiceAccounts) Present(ctx context.Context, ref clusterprovider.Ref, cred *clusterprovider.Credential) (bool, error) {
	core, err := identityCore(ref, cred)
	if err != nil {
		return false, err
	}
	_, err = core.CoreV1().ServiceAccounts(kcpclient.TenantNamespace).Get(ctx, kcpclient.ServiceAccountName(ref.DID), metav1.GetOptions{})
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("clusterprovider: read the ServiceAccount of %s in %s: %w", ref.DID, ref.Name, err)
	}
}

func (IdentityServiceAccounts) Delete(ctx context.Context, ref clusterprovider.Ref, cred *clusterprovider.Credential) error {
	core, err := identityCore(ref, cred)
	if err != nil {
		return err
	}
	err = core.CoreV1().ServiceAccounts(kcpclient.TenantNamespace).Delete(ctx, kcpclient.ServiceAccountName(ref.DID), metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clusterprovider: delete the ServiceAccount of %s in %s: %w", ref.DID, ref.Name, err)
	}
	return nil
}

func identityCore(ref clusterprovider.Ref, cred *clusterprovider.Credential) (kubernetes.Interface, error) {
	if ref.DID == "" {
		return nil, fmt.Errorf("clusterprovider: cluster %s names no owning DID, so its ServiceAccount has no name", ref.Name)
	}
	if cred == nil {
		return nil, fmt.Errorf("clusterprovider: no tenant credential for %s", ref.Name)
	}
	return kubernetes.NewForConfig(RestConfig(cred))
}

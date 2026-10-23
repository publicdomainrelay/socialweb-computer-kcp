package clusterprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const (
	PSAEnforceLabel = "pod-security.kubernetes.io/enforce"
	PSAWarnLabel    = "pod-security.kubernetes.io/warn"

	PSAEnforceValue = "baseline"
	PSAWarnValue    = "restricted"

	DefaultServiceAccountName = "default"

	serviceAccountAttempts = 3
)

var ErrNoHardeningCredential = errors.New(
	"clusterprovider: no client for the cluster's own apiserver was supplied, so its PSA labels " +
		"and its default ServiceAccount's automountServiceAccountToken=false can neither be " +
		"written nor read back; a cluster whose hardening is not in force is not one this " +
		"provider will hold at Ready")

var ErrHardeningNotInForce = errors.New(
	"clusterprovider: the cluster answered and its hardening is NOT in force")

var ErrHardeningRaced = errors.New(
	"clusterprovider: the default ServiceAccount appeared between the read and the write on " +
		"every attempt, so automountServiceAccountToken=false has nowhere to land")

var ErrNoHardeningSeam = errors.New(
	"clusterprovider: this Stack puts no k3s step in the group, so the hardening has no gate to " +
		"run behind and would SILENTLY NOT RUN -- a cluster hardened by nothing and a cluster " +
		"hardened by this step look identical from a green suite")

func TenantNamespaces() []string {
	return []string{kcpclient.TenantNamespace}
}

func psaLabels() map[string]string {
	return map[string]string{
		PSAEnforceLabel: PSAEnforceValue,
		PSAWarnLabel:    PSAWarnValue,
	}
}

func labelsCarryPSA(labels map[string]string) bool {
	for k, want := range psaLabels() {
		if labels[k] != want {
			return false
		}
	}
	return true
}

func hardenedServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: DefaultServiceAccountName, Namespace: namespace},
		AutomountServiceAccountToken: new(false),
	}
}

func Harden(ctx context.Context, core kubernetes.Interface) error {
	if core == nil {
		return ErrNoHardeningCredential
	}
	for _, ns := range TenantNamespaces() {
		if err := hardenNamespace(ctx, core, ns); err != nil {
			return err
		}
	}
	return nil
}

func hardenNamespace(ctx context.Context, core kubernetes.Interface, namespace string) error {
	if err := ensureNamespace(ctx, core, namespace); err != nil {
		return err
	}
	if err := ensureDefaultServiceAccount(ctx, core, namespace); err != nil {
		return err
	}
	missing := missingHardening(ctx, core, namespace)
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrHardeningNotInForce, strings.Join(missing, "; "))
	}
	return nil
}

func ensureNamespace(ctx context.Context, core kubernetes.Interface, namespace string) error {
	got, err := core.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: psaLabels()},
		}, metav1.CreateOptions{FieldManager: fieldManager})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("clusterprovider: creating namespace %s: %w", namespace, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("clusterprovider: reading namespace %s: %w", namespace, err)
	}
	if labelsCarryPSA(got.Labels) {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": namespace, "labels": psaLabels()},
	})
	if err != nil {
		return fmt.Errorf("clusterprovider: rendering the PSA labels for %s: %w", namespace, err)
	}
	if _, err := core.CoreV1().Namespaces().Patch(ctx, namespace, types.MergePatchType, body, metav1.PatchOptions{FieldManager: fieldManager}); err != nil {
		return fmt.Errorf("clusterprovider: labelling namespace %s: %w", namespace, err)
	}
	return nil
}

func ensureDefaultServiceAccount(ctx context.Context, core kubernetes.Interface, namespace string) error {
	for range serviceAccountAttempts {
		sa, err := core.CoreV1().ServiceAccounts(namespace).Get(ctx, DefaultServiceAccountName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, cerr := core.CoreV1().ServiceAccounts(namespace).Create(ctx, hardenedServiceAccount(namespace), metav1.CreateOptions{FieldManager: fieldManager})
			switch {
			case cerr == nil:
				return nil
			case apierrors.IsAlreadyExists(cerr):
				continue
			default:
				return fmt.Errorf("clusterprovider: creating the default ServiceAccount in %s: %w", namespace, cerr)
			}
		case err != nil:
			return fmt.Errorf("clusterprovider: reading the default ServiceAccount in %s: %w", namespace, err)
		}
		if sa.AutomountServiceAccountToken != nil && !*sa.AutomountServiceAccountToken {
			return nil
		}
		sa = sa.DeepCopy()
		sa.AutomountServiceAccountToken = new(false)
		if _, err := core.CoreV1().ServiceAccounts(namespace).Update(ctx, sa, metav1.UpdateOptions{FieldManager: fieldManager}); err != nil {
			return fmt.Errorf("clusterprovider: setting automountServiceAccountToken=false on the default ServiceAccount in %s: %w", namespace, err)
		}
		return nil
	}
	return fmt.Errorf("%w (%s)", ErrHardeningRaced, namespace)
}

type labelWant struct {
	label string
	value string
}

var psaWants = []labelWant{
	{PSAEnforceLabel, PSAEnforceValue},
	{PSAWarnLabel, PSAWarnValue},
}

func missingHardening(ctx context.Context, core kubernetes.Interface, namespace string) []string {
	var missing []string

	ns, err := core.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	switch {
	case err != nil:
		missing = append(missing, fmt.Sprintf("namespace %s: unreadable: %v", namespace, err))
	default:
		for _, w := range psaWants {
			if got := ns.Labels[w.label]; got != w.value {
				missing = append(missing, fmt.Sprintf("namespace %s %s=%q want %q", namespace, w.label, got, w.value))
			}
		}
	}

	sa, err := core.CoreV1().ServiceAccounts(namespace).Get(ctx, DefaultServiceAccountName, metav1.GetOptions{})
	switch {
	case err != nil:
		missing = append(missing, fmt.Sprintf("namespace %s default ServiceAccount: does not exist -- every pod in it gets an API token it did not ask for", namespace))
	case sa.AutomountServiceAccountToken == nil:
		missing = append(missing, fmt.Sprintf("namespace %s default ServiceAccount has no automountServiceAccountToken field, which means the cluster default (true)", namespace))
	case *sa.AutomountServiceAccountToken:
		missing = append(missing, fmt.Sprintf("namespace %s default ServiceAccount still automounts a token", namespace))
	}
	return missing
}

func hardenAfterK3s(steps []procgroup.Step, admin AdminClient, cluster, apiserver string) ([]procgroup.Step, error) {
	idx := -1
	for i, s := range steps {
		if s.Name == procgroup.ComponentK3s {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, ErrNoHardeningSeam
	}
	if admin == nil {
		return nil, ErrNoHardeningCredential
	}
	inner := steps[idx].Ready
	steps[idx].Ready = func(ctx context.Context, p procgroup.Member) error {
		if inner != nil {
			if err := inner(ctx, p); err != nil {
				return err
			}
		}
		core, err := admin(ctx, cluster, apiserver)
		if err != nil {
			return err
		}
		if err := Harden(ctx, core); err != nil {
			return fmt.Errorf("clusterprovider: %s answered at %s and %w", cluster, apiserver, err)
		}
		return nil
	}
	return steps, nil
}

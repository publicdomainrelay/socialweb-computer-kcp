package compose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const RootBindTimeout = 3 * time.Minute

var RootExports = []string{ExportApplications, ExportPolicy, ExportExposures, ExportPDS, ExportEvents, ExportHostProcesses}

var APIBindingGVR = kcpgvr.APIBindings

func BindingReady(b *apisv1alpha2.APIBinding) bool {
	if b == nil {
		return false
	}
	for _, c := range b.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return true
		}
	}
	return false
}

func unbound(items []apisv1alpha2.APIBinding, exports []string) []string {
	ready := map[string]bool{}
	for i := range items {
		if ref := items[i].Spec.Reference.Export; ref != nil && BindingReady(&items[i]) {
			ready[ref.Name] = true
		}
	}
	return slices.DeleteFunc(slices.Clone(exports), func(e string) bool { return ready[e] })
}

func AwaitBound(ctx context.Context, bindings kube.Resource, timeout time.Duration, exports ...string) error {
	seen, err := kube.AwaitList(ctx, bindings, metav1.ListOptions{}, timeout, func(items []apisv1alpha2.APIBinding) (bool, error) {
		return len(unbound(items, exports)) == 0, nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return fmt.Errorf("compose: no Ready APIBinding for the exports %s within %s", strings.Join(unbound(seen, exports), ", "), timeout)
	}
	return err
}

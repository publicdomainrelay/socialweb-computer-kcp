package eventstream

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type repoBackend struct {
	DID string

	PdsRef commonv1.ObjectRef
}

func readRepoBackend(ctx context.Context, dyn dynamic.Interface, repo v1alpha1.AtprotoRepo) (repoBackend, error) {
	ref := repo.Spec.BackendRef
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return repoBackend{}, fmt.Errorf("AtprotoRepo %s names no backendRef", repo.Name)
	}
	if dyn == nil {
		return repoBackend{}, errors.New("no dynamic client for the AtprotoRepo backend")
	}
	switch strings.TrimSpace(ref.Kind) {
	case "LocalRepo":
		u, err := dyn.Resource(v1alpha1.LocalRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return repoBackend{}, fmt.Errorf("reading LocalRepo %s: %w", name, err)
		}
		var local v1alpha1.LocalRepo
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &local); err != nil {
			return repoBackend{}, fmt.Errorf("decoding LocalRepo %s: %w", name, err)
		}
		return repoBackend{DID: strings.TrimSpace(local.Spec.DID), PdsRef: local.Spec.PdsRef}, nil
	case "RemoteRepo":
		u, err := dyn.Resource(v1alpha1.RemoteRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return repoBackend{}, fmt.Errorf("reading RemoteRepo %s: %w", name, err)
		}
		var remote v1alpha1.RemoteRepo
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &remote); err != nil {
			return repoBackend{}, fmt.Errorf("decoding RemoteRepo %s: %w", name, err)
		}
		return repoBackend{DID: strings.TrimSpace(remote.Spec.DID), PdsRef: remote.Spec.PdsRef}, nil
	}
	return repoBackend{}, fmt.Errorf("AtprotoRepo %s backendRef kind %q is neither LocalRepo nor RemoteRepo", repo.Name, ref.Kind)
}

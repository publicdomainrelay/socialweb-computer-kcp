package pdsclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type RepoBackend struct {
	Workspace string

	Kind string

	Name string

	DID string

	PdsRef commonv1.ObjectRef

	AuthRef *commonv1.SecretRef
}

func (b RepoBackend) Binding() RepoBinding {
	binding := RepoBinding{
		Workspace: b.PdsRef.InWorkspace(b.Workspace),
		Kind:      strings.TrimSpace(b.PdsRef.Kind),
		Name:      strings.TrimSpace(b.PdsRef.Name),
		DID:       strings.TrimSpace(b.DID),
	}
	if b.AuthRef != nil {
		binding.AuthRef = b.AuthRef.DeepCopy()
	}
	return binding
}

func ReadRepoBackend(ctx context.Context, dyn dynamic.Interface, workspace string, repo v1alpha1.AtprotoRepo) (RepoBackend, error) {
	ref := repo.Spec.BackendRef
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return RepoBackend{}, fmt.Errorf("AtprotoRepo %s names no backendRef", repo.Name)
	}
	if dyn == nil {
		return RepoBackend{}, errors.New("no dynamic client for the AtprotoRepo backend")
	}
	switch strings.TrimSpace(ref.Kind) {
	case "LocalRepo":
		u, err := dyn.Resource(v1alpha1.LocalRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return RepoBackend{}, fmt.Errorf("reading LocalRepo %s: %w", name, err)
		}
		var local v1alpha1.LocalRepo
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &local); err != nil {
			return RepoBackend{}, fmt.Errorf("decoding LocalRepo %s: %w", name, err)
		}
		return localRepoBackend(workspace, local), nil
	case "RemoteRepo":
		u, err := dyn.Resource(v1alpha1.RemoteRepoGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return RepoBackend{}, fmt.Errorf("reading RemoteRepo %s: %w", name, err)
		}
		var remote v1alpha1.RemoteRepo
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &remote); err != nil {
			return RepoBackend{}, fmt.Errorf("decoding RemoteRepo %s: %w", name, err)
		}
		return remoteRepoBackend(workspace, remote), nil
	default:
		return RepoBackend{}, fmt.Errorf("AtprotoRepo %s backendRef kind %q is neither LocalRepo nor RemoteRepo", repo.Name, ref.Kind)
	}
}

func ListRepoBackends(ctx context.Context, dyn dynamic.Interface, workspace string) ([]RepoBackend, error) {
	if dyn == nil {
		return nil, nil
	}
	var out []RepoBackend
	locals, err := dyn.Resource(v1alpha1.LocalRepoGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return nil, err
	}
	if err == nil {
		for i := range locals.Items {
			var local v1alpha1.LocalRepo
			if runtime.DefaultUnstructuredConverter.FromUnstructured(locals.Items[i].Object, &local) == nil {
				out = append(out, localRepoBackend(workspace, local))
			}
		}
	}
	remotes, err := dyn.Resource(v1alpha1.RemoteRepoGVR).List(ctx, metav1.ListOptions{})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) {
		return nil, err
	}
	if err == nil {
		for i := range remotes.Items {
			var remote v1alpha1.RemoteRepo
			if runtime.DefaultUnstructuredConverter.FromUnstructured(remotes.Items[i].Object, &remote) == nil {
				out = append(out, remoteRepoBackend(workspace, remote))
			}
		}
	}
	return out, nil
}

func localRepoBackend(workspace string, local v1alpha1.LocalRepo) RepoBackend {
	return RepoBackend{Workspace: workspace, Kind: "LocalRepo", Name: local.Name, DID: strings.TrimSpace(local.Spec.DID),
		PdsRef: local.Spec.PdsRef, AuthRef: local.Spec.AuthRef}
}

func remoteRepoBackend(workspace string, remote v1alpha1.RemoteRepo) RepoBackend {
	return RepoBackend{Workspace: workspace, Kind: "RemoteRepo", Name: remote.Name, DID: strings.TrimSpace(remote.Spec.DID),
		PdsRef: remote.Spec.PdsRef, AuthRef: remote.Spec.AuthRef}
}

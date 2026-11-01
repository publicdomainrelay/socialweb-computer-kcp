package embedded

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
)

const (
	ProviderIdentitiesVersion = 1

	providerWorkspacePhase = "workspace"
)

type ProviderIdentity struct {
	Export string `json:"export"`

	Namespace string `json:"namespace"`

	Name string `json:"name"`

	Type corev1.SecretType `json:"type,omitempty"`

	Data map[string][]byte `json:"data"`
}

type ProviderIdentities struct {
	Version int `json:"version"`

	Identities []ProviderIdentity `json:"identities"`
}

type apiExportIdentity struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		Identity *struct {
			SecretRef *corev1.SecretReference `json:"secretRef,omitempty"`
		} `json:"identity,omitempty"`
	} `json:"spec"`
}

func ParseProviderIdentities(raw []byte) (ProviderIdentities, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ProviderIdentities{}, nil
	}
	var out ProviderIdentities
	if err := json.Unmarshal(raw, &out); err != nil {
		return ProviderIdentities{}, errors.New("embedded: the provider identities are not valid JSON")
	}
	if out.Version != ProviderIdentitiesVersion {
		return ProviderIdentities{}, fmt.Errorf("embedded: unsupported provider identities version %d (want %d)", out.Version, ProviderIdentitiesVersion)
	}
	for _, id := range out.Identities {
		if id.Namespace == "" || id.Name == "" || len(id.Data) == 0 {
			return ProviderIdentities{}, fmt.Errorf("embedded: the provider identity of %q names no secret or holds no key", id.Export)
		}
	}
	return out, nil
}

func (r *Requester) ProviderIdentities(ctx context.Context) ([]byte, error) {
	if r == nil || r.rest == nil {
		return nil, errors.New("embedded: the requester is not running")
	}
	kc, err := kube.ForWorkspace(r.rest, r.providerWorkspace)
	if err != nil {
		return nil, err
	}
	exports, err := kube.List[apiExportIdentity](ctx, kc.Resource(kcpgvr.APIExports), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("embedded: listing the provider APIExports: %w", err)
	}
	out := ProviderIdentities{Version: ProviderIdentitiesVersion}
	for _, export := range exports {
		if export.Spec.Identity == nil || export.Spec.Identity.SecretRef == nil {
			return nil, fmt.Errorf("embedded: the APIExport %s has no identity yet", export.Metadata.Name)
		}
		ref := export.Spec.Identity.SecretRef
		secret, err := kube.Get[corev1.Secret](ctx, kc.Resource(kcpgvr.Secrets).In(ref.Namespace), ref.Name)
		if err != nil {
			return nil, fmt.Errorf("embedded: reading the identity of APIExport %s: %w", export.Metadata.Name, err)
		}
		out.Identities = append(out.Identities, ProviderIdentity{Export: export.Metadata.Name, Namespace: ref.Namespace, Name: ref.Name, Type: secret.Type, Data: secret.Data})
	}
	slices.SortFunc(out.Identities, func(a, b ProviderIdentity) int {
		return cmp.Compare(a.Export, b.Export)
	})
	return json.Marshal(out)
}

func restoreProviderIdentities(ctx context.Context, restCfg *rest.Config, workspace string, identities ProviderIdentities, log *slog.Logger) error {
	if len(identities.Identities) == 0 {
		return nil
	}
	kc, err := kube.ForWorkspace(restCfg, workspace)
	if err != nil {
		return err
	}
	namespaces := map[string]bool{}
	created, kept := 0, 0
	for _, id := range identities.Identities {
		secrets := kc.Resource(kcpgvr.Secrets).In(id.Namespace)
		live, err := kube.Get[corev1.Secret](ctx, secrets, id.Name)
		switch {
		case err == nil:
			if !maps.EqualFunc(live.Data, id.Data, bytes.Equal) {
				return fmt.Errorf("embedded: the provider workspace holds a different identity for APIExport %s than the one restored; refusing to start", id.Export)
			}
			kept++
			continue
		case !apierrors.IsNotFound(err):
			return fmt.Errorf("embedded: reading the identity of APIExport %s: %w", id.Export, err)
		}
		if !namespaces[id.Namespace] {
			ns := &corev1.Namespace{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"}, ObjectMeta: metav1.ObjectMeta{Name: id.Namespace}}
			if _, err := kube.Create[corev1.Namespace](ctx, kc.Resource(kcpgvr.Namespaces), ns, metav1.CreateOptions{FieldManager: FieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("embedded: creating the %s namespace for the provider identities: %w", id.Namespace, err)
			}
			namespaces[id.Namespace] = true
		}
		secret := &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: id.Name, Namespace: id.Namespace},
			Type:       id.Type,
			Data:       id.Data,
		}
		if _, err := kube.Create[corev1.Secret](ctx, secrets, secret, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
			return fmt.Errorf("embedded: restoring the identity of APIExport %s: %w", id.Export, err)
		}
		created++
	}
	log.Info("embedded: provider export identities restored", "created", created, "kept", kept)
	return nil
}

func splitAfterPhase(set manifests.Set, phase string) []manifests.Set {
	for i, p := range set.Phases {
		if p.Name != phase {
			continue
		}
		head, tail := set, set
		head.Phases = slices.Clone(set.Phases[:i+1])
		tail.Phases = slices.Clone(set.Phases[i+1:])
		return []manifests.Set{head, tail}
	}
	return []manifests.Set{set}
}

func endsWithPhase(set manifests.Set, phase string) bool {
	return len(set.Phases) > 0 && set.Phases[len(set.Phases)-1].Name == phase
}

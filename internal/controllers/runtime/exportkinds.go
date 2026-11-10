package runtime

import (
	"context"
	"maps"
	"slices"
	"time"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const ExportKindsTimeout = time.Minute

func (m *Manager) addKind(export string, obj client.Object) {
	gvks, _, err := m.scheme.ObjectKinds(obj)
	if err != nil || len(gvks) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.kinds[export] == nil {
		m.kinds[export] = map[schema.GroupKind]bool{}
	}
	m.kinds[export][gvks[0].GroupKind()] = true
}

func (m *Manager) awaitKinds(ctx context.Context, export string) {
	m.mu.Lock()
	want := maps.Clone(m.kinds[export])
	m.mu.Unlock()
	if len(want) == 0 {
		return
	}
	c, err := kube.New(clusterConfig(m.base, m.exportWorkspace))
	if err != nil {
		m.log.Warn("the APIExport provider does not wait for its kinds", "export", export, "err", err.Error())
		return
	}
	schemas := c.Resource(kcpgvr.APIResourceSchemas)
	known := map[string]schema.GroupKind{}
	var resources []apisv1alpha2.ResourceSchema
	var left []string
	waiting := false
	_, err = kube.AwaitList(ctx, c.Resource(kcpgvr.APIExports), metav1.ListOptions{}, ExportKindsTimeout, func(items []apisv1alpha2.APIExport) (bool, error) {
		i := slices.IndexFunc(items, func(e apisv1alpha2.APIExport) bool { return e.Name == export })
		served := map[schema.GroupKind]bool{}
		if i >= 0 {
			resources = items[i].Spec.Resources
			for _, r := range resources {
				gk, ok := known[r.Schema]
				if !ok {
					s, err := kube.Get[apisv1alpha1.APIResourceSchema](ctx, schemas, r.Schema)
					if err != nil {
						continue
					}
					gk = schema.GroupKind{Group: s.Spec.Group, Kind: s.Spec.Names.Kind}
					known[r.Schema] = gk
				}
				served[gk] = true
			}
		}
		left = left[:0]
		for gk := range want {
			if !served[gk] {
				left = append(left, gk.String())
			}
		}
		if len(left) == 0 {
			return true, nil
		}
		if !waiting {
			waiting = true
			slices.Sort(left)
			m.log.Info("the APIExport provider waits for its kinds; the schema apply wakes it", "export", export, "kinds", left)
		}
		return false, nil
	})
	if err != nil {
		m.log.Warn("the APIExport provider starts without all its kinds", "export", export, "kinds", left, "err", err.Error())
		return
	}
	for _, ws := range m.boundIn {
		m.awaitBound(ctx, ws, export, resources)
	}
	if waiting {
		m.log.Info("the APIExport serves its kinds", "export", export)
	}
}

func (m *Manager) awaitBound(ctx context.Context, ws, export string, resources []apisv1alpha2.ResourceSchema) {
	c, err := kube.New(clusterConfig(m.base, ws))
	if err != nil {
		m.log.Warn("the APIExport provider does not wait for its binding", "export", export, "workspace", ws, "err", err.Error())
		return
	}
	bindings := c.Resource(kcpgvr.APIBindings)
	boundAll := func(items []apisv1alpha2.APIBinding) (bool, error) {
		for _, b := range items {
			if ref := b.Spec.Reference.Export; ref == nil || ref.Name != export {
				continue
			}
			bound := map[schema.GroupResource]bool{}
			for _, r := range b.Status.BoundResources {
				bound[schema.GroupResource{Group: r.Group, Resource: r.Resource}] = true
			}
			for _, r := range resources {
				if !bound[schema.GroupResource{Group: r.Group, Resource: r.Name}] {
					return false, nil
				}
			}
		}
		return true, nil
	}
	items, err := kube.List[apisv1alpha2.APIBinding](ctx, bindings, metav1.ListOptions{})
	if err != nil {
		return
	}
	if done, _ := boundAll(items); done {
		return
	}
	if _, err := kube.AwaitList(ctx, bindings, metav1.ListOptions{}, ExportKindsTimeout, boundAll); err != nil {
		m.log.Warn("the APIExport provider starts before its binding serves every kind", "export", export, "workspace", ws, "err", err.Error())
	}
}

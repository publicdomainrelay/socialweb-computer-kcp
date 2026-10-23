package clusterprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eso"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

var ErrNoBootstrap = errors.New("clusterprovider: the tenant bootstrap needs a tenant credential and a grant path")

const fieldManager = "socialweb-clusterprovider"

var grantResources = map[string]schema.GroupVersionResource{
	"ClusterRoleBinding":               {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
	"ValidatingAdmissionPolicy":        {Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"},
	"ValidatingAdmissionPolicyBinding": {Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"},
}

func GrantObjects(doc []byte) ([]unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
	var out []unstructured.Unstructured
	for {
		var raw map[string]any
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("clusterprovider: the grant document: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		u := unstructured.Unstructured{Object: raw}
		gvk := u.GroupVersionKind()
		gvr, ok := grantResources[gvk.Kind]
		if !ok {
			return nil, fmt.Errorf(
				"clusterprovider: the grant document holds a %q, which is not one of the %d kinds "+
					"this bootstrap applies; a new kind is a decision (see grantResources), not an apply",
				gvk.Kind, len(grantResources))
		}
		if want := gvr.GroupVersion().String(); gvk.GroupVersion().String() != want {
			return nil, fmt.Errorf(
				"clusterprovider: the grant's %s is %s and this bootstrap applies it as %s",
				gvk.Kind, gvk.GroupVersion().String(), want)
		}
		if u.GetName() == "" {
			return nil, fmt.Errorf("clusterprovider: the grant holds a %s with no name", gvk.Kind)
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil, errors.New("clusterprovider: the grant document holds no objects")
	}
	return out, nil
}

func applyGrant(ctx context.Context, dyn dynamic.Interface, objs []unstructured.Unstructured) error {
	for i := range objs {
		u := &objs[i]
		gvr := grantResources[u.GetKind()]
		body, err := json.Marshal(u.Object)
		if err != nil {
			return fmt.Errorf("clusterprovider: marshalling the %s %s: %w", u.GetKind(), u.GetName(), err)
		}
		_, err = dyn.Resource(gvr).Namespace(u.GetNamespace()).Patch(
			ctx, u.GetName(), types.ApplyPatchType, body,
			metav1.PatchOptions{FieldManager: fieldManager, Force: new(true)},
		)
		if err != nil {
			return fmt.Errorf("clusterprovider: applying the %s %s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

func (p *Provider) Bootstrap(ctx context.Context, ref Ref, cl *v1alpha1.Cluster) error {
	if p.opts.Tenant == nil || p.opts.GrantPath == "" {
		return fmt.Errorf("%w (cluster %s)", ErrNoBootstrap, ref.Name)
	}
	key, err := ref.Key()
	if err != nil {
		return err
	}
	cfg, err := p.opts.Tenant(ctx, ref, cl)
	if err != nil {
		return fmt.Errorf("clusterprovider: no tenant credential for %s: %w", ref.Name, err)
	}
	if cfg == nil {
		return fmt.Errorf("clusterprovider: no tenant credential for %s", ref.Name)
	}

	raw, err := os.ReadFile(p.opts.GrantPath)
	if err != nil {
		return fmt.Errorf("clusterprovider: reading the tenant grant: %w", err)
	}
	grant, err := kcpclient.RenderTenantGrant(string(raw), kcpclient.GrantScope{
		OwnerDID: ref.DID,
		ESOUser:  eso.ControllerUser,
	})
	if err != nil {
		return fmt.Errorf("clusterprovider: the tenant grant for %s: %w", ref.Name, err)
	}
	objs, err := GrantObjects([]byte(grant))
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(kcpserver.Throughput(cfg))
	if err != nil {
		return fmt.Errorf("clusterprovider: a dynamic client for %s: %w", ref.Name, err)
	}
	if err := applyGrant(ctx, dyn, objs); err != nil {
		return err
	}
	if p.opts.InstallTenant != nil {
		if err := p.opts.InstallTenant(ctx, key, cfg); err != nil {
			return fmt.Errorf("clusterprovider: installing the tenant add-ons for %s after its "+
				"bootstrap: %w", ref.Name, err)
		}
	}
	p.opts.Log.Info("tenant bootstrap complete",
		"cluster", ref.Name, "key", key, "did", ref.DID,
		"grant", p.opts.GrantPath, "objects", len(objs),
		"credentialHost", cfg.Host, "statusURL", cl.Status.URL,
	)
	return nil
}

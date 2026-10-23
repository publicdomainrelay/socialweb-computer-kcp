package clusterkcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eso"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const fieldManager = "socialweb-clusterprovider"

var grantResources = map[string]schema.GroupVersionResource{
	"ClusterRoleBinding":               kcpgvr.ClusterRoleBindings,
	"ValidatingAdmissionPolicy":        kcpgvr.ValidatingAdmissionPolicies,
	"ValidatingAdmissionPolicyBinding": kcpgvr.ValidatingAdmissionPolicyBindings,
}

func Grant(ctx context.Context, ref clusterprovider.Ref, cred *clusterprovider.Credential, doc []byte) (int, error) {
	grant, err := kcpclient.RenderTenantGrant(string(doc), kcpclient.GrantScope{
		OwnerDID: ref.DID,
		ESOUser:  eso.ControllerUser,
	})
	if err != nil {
		return 0, fmt.Errorf("clusterprovider: the tenant grant for %s: %w", ref.Name, err)
	}
	objs, err := GrantObjects([]byte(grant))
	if err != nil {
		return 0, err
	}
	kc, err := kube.New(kcpserver.Throughput(RestConfig(cred)))
	if err != nil {
		return 0, fmt.Errorf("clusterprovider: a dynamic client for %s: %w", ref.Name, err)
	}
	if err := applyGrant(ctx, kc, objs); err != nil {
		return 0, err
	}
	return len(objs), nil
}

func GrantObjects(doc []byte) ([]kube.Doc, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), 4096)
	var out []kube.Doc
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
		gvk := kindOf(raw)
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
		if metadataString(raw, "name") == "" {
			return nil, fmt.Errorf("clusterprovider: the grant holds a %s with no name", gvk.Kind)
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return nil, errors.New("clusterprovider: the grant document holds no objects")
	}
	return out, nil
}

func kindOf(d kube.Doc) schema.GroupVersionKind {
	av, _ := d["apiVersion"].(string)
	gv, err := schema.ParseGroupVersion(av)
	if err != nil {
		return schema.GroupVersionKind{}
	}
	kind, _ := d["kind"].(string)
	return gv.WithKind(kind)
}

func metadataString(d kube.Doc, field string) string {
	meta, _ := d["metadata"].(map[string]any)
	v, _ := meta[field].(string)
	return v
}

func applyGrant(ctx context.Context, kc *kube.Client, objs []kube.Doc) error {
	for _, d := range objs {
		kind, _ := d["kind"].(string)
		name := metadataString(d, "name")
		r := kc.Resource(grantResources[kind]).In(metadataString(d, "namespace"))
		if _, err := kube.Apply[kube.Doc](ctx, r, d, kube.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			return fmt.Errorf("clusterprovider: applying the %s %s: %w", kind, name, err)
		}
	}
	return nil
}

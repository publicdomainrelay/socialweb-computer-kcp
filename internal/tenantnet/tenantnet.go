package tenantnet

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

//go:embed manifests/*.yaml
var manifests embed.FS

const (
	Address = "169.254.20.10"

	NodeLabel = "socialweb.computer/node-local-dns"

	DNSName = "socialweb-node-dns"

	NodeReaderName = "socialweb-meshd-node-reader"
)

var ManifestOrder = []string{
	"manifests/node-reader-rbac.yaml",
	"manifests/node-local-dns.yaml",
}

var ErrUnknownKind = errors.New("tenantnet: a manifest names a kind this package does not apply")

var resources = map[string]schema.GroupVersionResource{
	"ServiceAccount":     kcpgvr.ServiceAccounts,
	"ConfigMap":          kcpgvr.ConfigMaps,
	"ClusterRole":        kcpgvr.ClusterRoles,
	"ClusterRoleBinding": kcpgvr.ClusterRoleBindings,
	"DaemonSet":          kcpgvr.DaemonSets,
}

func Objects() ([]kube.Doc, error) {
	var out []kube.Doc
	for _, name := range ManifestOrder {
		raw, err := manifests.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("tenantnet: reading %s: %w", name, err)
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		for {
			var obj map[string]any
			if err := dec.Decode(&obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return nil, fmt.Errorf("tenantnet: decoding %s: %w", name, err)
			}
			if len(obj) == 0 {
				continue
			}
			u := kube.Doc(obj)
			if _, ok := resources[str(u, "kind")]; !ok {
				return nil, fmt.Errorf("%w: %s %s in %s", ErrUnknownKind, str(u, "kind"), str(u, "metadata", "name"), name)
			}
			out = append(out, u)
		}
	}
	return out, nil
}

func Apply(ctx context.Context, c *kube.Client, manager string) error {
	objs, err := Objects()
	if err != nil {
		return err
	}
	for _, u := range objs {
		kind, name := str(u, "kind"), str(u, "metadata", "name")
		r := c.Resource(resources[kind]).In(str(u, "metadata", "namespace"))
		if _, err := kube.Apply[kube.Doc](ctx, r, u, kube.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
			return fmt.Errorf("tenantnet: applying the %s %s: %w", kind, name, err)
		}
	}
	return nil
}

func str(d kube.Doc, fields ...string) string {
	var v any = map[string]any(d)
	for _, f := range fields {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[f]
	}
	s, _ := v.(string)
	return s
}

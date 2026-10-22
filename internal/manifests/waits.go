package manifests

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type WaitKind struct {
	Resource   schema.GroupVersionResource
	Namespaced bool
	For        map[string]string
}

var WaitKinds = map[string]WaitKind{
	"Workspace": {
		Resource: schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"},
		For:      map[string]string{"Ready": CaptureLogicalCluster},
	},
	"APIExport": {
		Resource: schema.GroupVersionResource{Group: "apis.kcp.io", Version: "v1alpha2", Resource: "apiexports"},
		For:      map[string]string{"IdentityValid": ""},
	},
	"APIBinding": {
		Resource: schema.GroupVersionResource{Group: "apis.kcp.io", Version: "v1alpha2", Resource: "apibindings"},
		For:      map[string]string{"Ready": ""},
	},
	"OpenBaoServer": {
		Resource: schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "openbaoservers"},
		For:      map[string]string{"Ready": ""},
	},
}

func waitKindNames() []string {
	out := make([]string, 0, len(WaitKinds))
	for k := range WaitKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func conditionTrue(u *unstructured.Unstructured, name string) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == name && m["status"] == "True" {
			return true
		}
	}
	return false
}

func (w Wait) Met(u *unstructured.Unstructured) (bool, string) {
	switch w.Kind + "/" + w.For {
	case "Workspace/Ready":
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		cluster, _, _ := unstructured.NestedString(u.Object, "spec", "cluster")
		return phase == "Ready", cluster
	case "APIExport/IdentityValid":
		return conditionTrue(u, "IdentityValid"), ""
	case "APIBinding/Ready":
		return conditionTrue(u, "Ready"), ""
	case "OpenBaoServer/Ready":
		return conditionTrue(u, "Ready"), ""
	}
	return false, ""
}

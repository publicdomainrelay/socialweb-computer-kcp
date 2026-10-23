package unstruct

import (
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

func To(o any, kind string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u, nil
}

func From[T any](u *unstructured.Unstructured) (T, error) {
	var o T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

func Into(u *unstructured.Unstructured, out any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, out)
}

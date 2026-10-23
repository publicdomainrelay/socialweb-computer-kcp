package kube

import (
	"fmt"
	"reflect"
	"sync"

	kruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

var Scheme = sync.OnceValue(func() *kruntime.Scheme {
	s := kruntime.NewScheme()
	for _, add := range []func(*kruntime.Scheme) error{
		clientgoscheme.AddToScheme, apisv1alpha1.AddToScheme, apisv1alpha2.AddToScheme, corev1alpha1.AddToScheme, tenancyv1alpha1.AddToScheme,
		v1alpha1.AddToScheme, eventsv1alpha1.AddToScheme, exposurev1alpha1.AddToScheme, pdsv1alpha1.AddToScheme, policyv1alpha1.AddToScheme,
	} {
		if err := add(s); err != nil {
			panic(fmt.Sprintf("kube: building the scheme: %v", err))
		}
	}
	return s
})

func asDoc(obj any) (Doc, bool) {
	switch v := obj.(type) {
	case map[string]any:
		return v, true
	case *map[string]any:
		if v == nil {
			return nil, false
		}
		return *v, true
	}
	return nil, false
}

func Encode(obj any) (Doc, error) {
	if obj == nil {
		return nil, fmt.Errorf("kube: encoding a nil object")
	}
	if d, ok := asDoc(obj); ok {
		return d, nil
	}
	if v := reflect.ValueOf(obj); v.Kind() != reflect.Pointer {
		ptr := reflect.New(v.Type())
		ptr.Elem().Set(v)
		obj = ptr.Interface()
	}
	m, err := kruntime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, fmt.Errorf("kube: encoding %T: %w", obj, err)
	}
	if av, _ := m["apiVersion"].(string); av != "" {
		if k, _ := m["kind"].(string); k != "" {
			return m, nil
		}
	}
	ro, ok := obj.(kruntime.Object)
	if !ok {
		return nil, fmt.Errorf("kube: %T carries no apiVersion and kind", obj)
	}
	gvks, _, err := Scheme().ObjectKinds(ro)
	if err != nil || len(gvks) == 0 {
		return nil, fmt.Errorf("kube: the kind of %T: %w", obj, err)
	}
	m["apiVersion"], m["kind"] = gvks[0].GroupVersion().String(), gvks[0].Kind
	return m, nil
}

func Decode[T any](doc Doc) (*T, error) {
	out := new(T)
	if p, ok := any(out).(*map[string]any); ok {
		*p = doc
		return out, nil
	}
	if err := kruntime.DefaultUnstructuredConverter.FromUnstructured(doc, out); err != nil {
		return nil, fmt.Errorf("kube: decoding into %T: %w", out, err)
	}
	return out, nil
}

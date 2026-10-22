package apigen

import (
	"reflect"

	eventsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type Group struct {
	Dir       string
	Package   string
	Group     string
	Version   string
	Resources []v1alpha1.APIResource
}

func Groups() []Group {
	return []Group{
		{
			Dir:       "api/v1alpha1",
			Package:   reflect.TypeOf(v1alpha1.APIResource{}).PkgPath(),
			Group:     v1alpha1.GroupName,
			Version:   v1alpha1.Version,
			Resources: v1alpha1.APIResources,
		},
		{
			Dir:       "api/policy/v1alpha1",
			Package:   reflect.TypeOf(policyv1alpha1.PolicyEngine{}).PkgPath(),
			Group:     policyv1alpha1.GroupName,
			Version:   policyv1alpha1.Version,
			Resources: policyv1alpha1.APIResources,
		},
		{
			Dir:       "api/exposure/v1alpha1",
			Package:   reflect.TypeOf(exposurev1alpha1.Service{}).PkgPath(),
			Group:     exposurev1alpha1.GroupName,
			Version:   exposurev1alpha1.Version,
			Resources: exposurev1alpha1.APIResources,
		},
		{
			Dir:       "api/pds/v1alpha1",
			Package:   reflect.TypeOf(pdsv1alpha1.RemotePDS{}).PkgPath(),
			Group:     pdsv1alpha1.GroupName,
			Version:   pdsv1alpha1.Version,
			Resources: pdsv1alpha1.APIResources,
		},
		{
			Dir:       "api/events/v1alpha1",
			Package:   reflect.TypeOf(eventsv1alpha1.EventStream{}).PkgPath(),
			Group:     eventsv1alpha1.GroupName,
			Version:   eventsv1alpha1.Version,
			Resources: eventsv1alpha1.APIResources,
		},
	}
}

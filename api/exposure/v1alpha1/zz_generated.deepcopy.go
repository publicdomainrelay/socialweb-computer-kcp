package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *Service) DeepCopyInto(out *Service) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Service) DeepCopy() *Service {
	if in == nil {
		return nil
	}
	out := new(Service)
	in.DeepCopyInto(out)
	return out
}

func (in *Service) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ServiceList) DeepCopyInto(out *ServiceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Service, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ServiceList) DeepCopy() *ServiceList {
	if in == nil {
		return nil
	}
	out := new(ServiceList)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ServiceSpec) DeepCopyInto(out *ServiceSpec) {
	*out = *in
	out.TargetRef = in.TargetRef
	in.Auth.DeepCopyInto(&out.Auth)
}

func (in *ServiceSpec) DeepCopy() *ServiceSpec {
	if in == nil {
		return nil
	}
	out := new(ServiceSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceTargetRef) DeepCopyInto(out *ServiceTargetRef) {
	*out = *in
}

func (in *ServiceTargetRef) DeepCopy() *ServiceTargetRef {
	if in == nil {
		return nil
	}
	out := new(ServiceTargetRef)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceAuth) DeepCopyInto(out *ServiceAuth) {
	*out = *in
	out.AllowedSubjects = copyStringSlice(in.AllowedSubjects)
}

func (in *ServiceAuth) DeepCopy() *ServiceAuth {
	if in == nil {
		return nil
	}
	out := new(ServiceAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *ServiceStatus) DeepCopyInto(out *ServiceStatus) {
	*out = *in
	out.Endpoints = copyStringSlice(in.Endpoints)
	out.Conditions = copyConditions(in.Conditions)
}

func (in *ServiceStatus) DeepCopy() *ServiceStatus {
	if in == nil {
		return nil
	}
	out := new(ServiceStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *Exposure) DeepCopyInto(out *Exposure) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Exposure) DeepCopy() *Exposure {
	if in == nil {
		return nil
	}
	out := new(Exposure)
	in.DeepCopyInto(out)
	return out
}

func (in *Exposure) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ExposureList) DeepCopyInto(out *ExposureList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Exposure, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ExposureList) DeepCopy() *ExposureList {
	if in == nil {
		return nil
	}
	out := new(ExposureList)
	in.DeepCopyInto(out)
	return out
}

func (in *ExposureList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ExposureSpec) DeepCopyInto(out *ExposureSpec) {
	*out = *in
	in.TLS.DeepCopyInto(&out.TLS)
	in.Auth.DeepCopyInto(&out.Auth)
}

func (in *ExposureSpec) DeepCopy() *ExposureSpec {
	if in == nil {
		return nil
	}
	out := new(ExposureSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *ExposureTLS) DeepCopyInto(out *ExposureTLS) {
	*out = *in
	out.Hostnames = copyStringSlice(in.Hostnames)
}

func (in *ExposureTLS) DeepCopy() *ExposureTLS {
	if in == nil {
		return nil
	}
	out := new(ExposureTLS)
	in.DeepCopyInto(out)
	return out
}

func (in *ExposureAuth) DeepCopyInto(out *ExposureAuth) {
	*out = *in
	out.AllowedDIDs = copyStringSlice(in.AllowedDIDs)
}

func (in *ExposureAuth) DeepCopy() *ExposureAuth {
	if in == nil {
		return nil
	}
	out := new(ExposureAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *ExposureStatus) DeepCopyInto(out *ExposureStatus) {
	*out = *in
	out.Conditions = copyConditions(in.Conditions)
}

func (in *ExposureStatus) DeepCopy() *ExposureStatus {
	if in == nil {
		return nil
	}
	out := new(ExposureStatus)
	in.DeepCopyInto(out)
	return out
}

func copyStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func copyConditions(in []metav1.Condition) []metav1.Condition {
	if in == nil {
		return nil
	}
	out := make([]metav1.Condition, len(in))
	for i := range in {
		in[i].DeepCopyInto(&out[i])
	}
	return out
}

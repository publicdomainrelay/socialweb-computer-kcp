package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *RemotePDS) DeepCopyInto(out *RemotePDS) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *RemotePDS) DeepCopy() *RemotePDS {
	if in == nil {
		return nil
	}
	out := new(RemotePDS)
	in.DeepCopyInto(out)
	return out
}

func (in *RemotePDS) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RemotePDSList) DeepCopyInto(out *RemotePDSList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]RemotePDS, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RemotePDSList) DeepCopy() *RemotePDSList {
	if in == nil {
		return nil
	}
	out := new(RemotePDSList)
	in.DeepCopyInto(out)
	return out
}

func (in *RemotePDSList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RemotePDSSpec) DeepCopyInto(out *RemotePDSSpec) {
	*out = *in
	out.TLS = in.TLS
	out.Auth = in.Auth
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
}

func (in *RemotePDSSpec) DeepCopy() *RemotePDSSpec {
	if in == nil {
		return nil
	}
	out := new(RemotePDSSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RemotePDSTLS) DeepCopyInto(out *RemotePDSTLS) {
	*out = *in
}

func (in *RemotePDSTLS) DeepCopy() *RemotePDSTLS {
	if in == nil {
		return nil
	}
	out := new(RemotePDSTLS)
	in.DeepCopyInto(out)
	return out
}

func (in *RemotePDSAuth) DeepCopyInto(out *RemotePDSAuth) {
	*out = *in
}

func (in *RemotePDSAuth) DeepCopy() *RemotePDSAuth {
	if in == nil {
		return nil
	}
	out := new(RemotePDSAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *RemotePDSStatus) DeepCopyInto(out *RemotePDSStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.LastCheckedAt != nil {
		out.LastCheckedAt = in.LastCheckedAt.DeepCopy()
	}
}

func (in *RemotePDSStatus) DeepCopy() *RemotePDSStatus {
	if in == nil {
		return nil
	}
	out := new(RemotePDSStatus)
	in.DeepCopyInto(out)
	return out
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *EventStream) DeepCopyInto(out *EventStream) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *EventStream) DeepCopy() *EventStream {
	if in == nil {
		return nil
	}
	out := new(EventStream)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStream) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *EventStreamList) DeepCopyInto(out *EventStreamList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]EventStream, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *EventStreamList) DeepCopy() *EventStreamList {
	if in == nil {
		return nil
	}
	out := new(EventStreamList)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStreamList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *EventStreamSpec) DeepCopyInto(out *EventStreamSpec) {
	*out = *in
	in.Source.DeepCopyInto(&out.Source)
	if in.Cursor != nil {
		out.Cursor = new(int64)
		*out.Cursor = *in.Cursor
	}
	out.Sink = in.Sink
}

func (in *EventStreamSpec) DeepCopy() *EventStreamSpec {
	if in == nil {
		return nil
	}
	out := new(EventStreamSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStreamSource) DeepCopyInto(out *EventStreamSource) {
	*out = *in
	if in.PDSRef != nil {
		out.PDSRef = new(EventStreamRef)
		*out.PDSRef = *in.PDSRef
	}
	if in.RelayRef != nil {
		out.RelayRef = new(EventStreamRef)
		*out.RelayRef = *in.RelayRef
	}
	if in.Collections != nil {
		out.Collections = make([]string, len(in.Collections))
		copy(out.Collections, in.Collections)
	}
	if in.DIDs != nil {
		out.DIDs = make([]string, len(in.DIDs))
		copy(out.DIDs, in.DIDs)
	}
}

func (in *EventStreamSource) DeepCopy() *EventStreamSource {
	if in == nil {
		return nil
	}
	out := new(EventStreamSource)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStreamRef) DeepCopyInto(out *EventStreamRef) {
	*out = *in
}

func (in *EventStreamRef) DeepCopy() *EventStreamRef {
	if in == nil {
		return nil
	}
	out := new(EventStreamRef)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStreamSink) DeepCopyInto(out *EventStreamSink) {
	*out = *in
}

func (in *EventStreamSink) DeepCopy() *EventStreamSink {
	if in == nil {
		return nil
	}
	out := new(EventStreamSink)
	in.DeepCopyInto(out)
	return out
}

func (in *EventStreamStatus) DeepCopyInto(out *EventStreamStatus) {
	*out = *in
	if in.LastEventAt != nil {
		out.LastEventAt = in.LastEventAt.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *EventStreamStatus) DeepCopy() *EventStreamStatus {
	if in == nil {
		return nil
	}
	out := new(EventStreamStatus)
	in.DeepCopyInto(out)
	return out
}

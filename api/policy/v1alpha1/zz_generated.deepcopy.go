package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *PolicyEngine) DeepCopyInto(out *PolicyEngine) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PolicyEngine) DeepCopy() *PolicyEngine {
	if in == nil {
		return nil
	}
	out := new(PolicyEngine)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngine) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyEngineList) DeepCopyInto(out *PolicyEngineList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PolicyEngine, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PolicyEngineList) DeepCopy() *PolicyEngineList {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineList)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngineList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyEngineSpec) DeepCopyInto(out *PolicyEngineSpec) {
	*out = *in
	in.Limits.DeepCopyInto(&out.Limits)
	in.Trust.DeepCopyInto(&out.Trust)
	in.Cache.DeepCopyInto(&out.Cache)
}

func (in *PolicyEngineSpec) DeepCopy() *PolicyEngineSpec {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngineLimits) DeepCopyInto(out *PolicyEngineLimits) {
	*out = *in
	if in.ActiveDeadlineSeconds != nil {
		v := *in.ActiveDeadlineSeconds
		out.ActiveDeadlineSeconds = &v
	}
}

func (in *PolicyEngineLimits) DeepCopy() *PolicyEngineLimits {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineLimits)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngineTrustConfig) DeepCopyInto(out *PolicyEngineTrustConfig) {
	*out = *in
	if in.TTLSeconds != nil {
		v := *in.TTLSeconds
		out.TTLSeconds = &v
	}
}

func (in *PolicyEngineTrustConfig) DeepCopy() *PolicyEngineTrustConfig {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineTrustConfig)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngineCacheConfig) DeepCopyInto(out *PolicyEngineCacheConfig) {
	*out = *in
	if in.VerdictTTLSeconds != nil {
		v := *in.VerdictTTLSeconds
		out.VerdictTTLSeconds = &v
	}
	if in.ScopeTTLSeconds != nil {
		v := *in.ScopeTTLSeconds
		out.ScopeTTLSeconds = &v
	}
	if in.RecordTTLSeconds != nil {
		v := *in.RecordTTLSeconds
		out.RecordTTLSeconds = &v
	}
}

func (in *PolicyEngineCacheConfig) DeepCopy() *PolicyEngineCacheConfig {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineCacheConfig)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyEngineStatus) DeepCopyInto(out *PolicyEngineStatus) {
	*out = *in
	out.Conditions = copyConditions(in.Conditions)
}

func (in *PolicyEngineStatus) DeepCopy() *PolicyEngineStatus {
	if in == nil {
		return nil
	}
	out := new(PolicyEngineStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflow) DeepCopyInto(out *PolicyWorkflow) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PolicyWorkflow) DeepCopy() *PolicyWorkflow {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflow)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflow) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyWorkflowList) DeepCopyInto(out *PolicyWorkflowList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PolicyWorkflow, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PolicyWorkflowList) DeepCopy() *PolicyWorkflowList {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowList)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflowList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyWorkflowSpec) DeepCopyInto(out *PolicyWorkflowSpec) {
	*out = *in
	out.Workflow = copyRawMessage(in.Workflow)
	if in.RecordRef != nil {
		out.RecordRef = in.RecordRef.DeepCopy()
	}
	out.DefaultInputs = copyStringMap(in.DefaultInputs)
	out.DefaultArgs = copyStringMap(in.DefaultArgs)
	if in.TimeoutSeconds != nil {
		v := *in.TimeoutSeconds
		out.TimeoutSeconds = &v
	}
	if in.TTLSecondsAfterFinished != nil {
		v := *in.TTLSecondsAfterFinished
		out.TTLSecondsAfterFinished = &v
	}
}

func (in *PolicyWorkflowSpec) DeepCopy() *PolicyWorkflowSpec {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflowStatus) DeepCopyInto(out *PolicyWorkflowStatus) {
	*out = *in
	out.Conditions = copyConditions(in.Conditions)
}

func (in *PolicyWorkflowStatus) DeepCopy() *PolicyWorkflowStatus {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyStrongRef) DeepCopyInto(out *PolicyStrongRef) {
	*out = *in
}

func (in *PolicyStrongRef) DeepCopy() *PolicyStrongRef {
	if in == nil {
		return nil
	}
	out := new(PolicyStrongRef)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyCacheFile) DeepCopyInto(out *PolicyCacheFile) {
	*out = *in
}

func (in *PolicyCacheFile) DeepCopy() *PolicyCacheFile {
	if in == nil {
		return nil
	}
	out := new(PolicyCacheFile)
	in.DeepCopyInto(out)
	return out
}

func (in PolicyCacheEntry) DeepCopyInto(out *PolicyCacheEntry) {
	if in == nil {
		*out = nil
		return
	}
	outMap := make(PolicyCacheEntry, len(in))
	for k, v := range in {
		outMap[k] = v
	}
	*out = outMap
}

func (in PolicyCacheEntry) DeepCopy() PolicyCacheEntry {
	if in == nil {
		return nil
	}
	out := new(PolicyCacheEntry)
	in.DeepCopyInto(out)
	return *out
}

func (in PolicyCache) DeepCopyInto(out *PolicyCache) {
	if in == nil {
		*out = nil
		return
	}
	outMap := make(PolicyCache, len(in))
	for k := range in {
		outMap[k] = in[k].DeepCopy()
	}
	*out = outMap
}

func (in PolicyCache) DeepCopy() PolicyCache {
	if in == nil {
		return nil
	}
	out := new(PolicyCache)
	in.DeepCopyInto(out)
	return *out
}

func (in *PolicyWorkflowRun) DeepCopyInto(out *PolicyWorkflowRun) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PolicyWorkflowRun) DeepCopy() *PolicyWorkflowRun {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowRun)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflowRun) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyWorkflowRunList) DeepCopyInto(out *PolicyWorkflowRunList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PolicyWorkflowRun, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PolicyWorkflowRunList) DeepCopy() *PolicyWorkflowRunList {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowRunList)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflowRunList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyWorkflowRunSpec) DeepCopyInto(out *PolicyWorkflowRunSpec) {
	*out = *in
	out.Workflow = copyRawMessage(in.Workflow)
	out.Inputs = copyStringMap(in.Inputs)
	out.Args = copyStringMap(in.Args)
	if in.RFP != nil {
		out.RFP = in.RFP.DeepCopy()
	}
	if in.Bid != nil {
		out.Bid = in.Bid.DeepCopy()
	}
	if in.Accept != nil {
		out.Accept = in.Accept.DeepCopy()
	}
	out.Cache = copyPolicyCache(in.Cache)
	if in.ActiveDeadlineSeconds != nil {
		v := *in.ActiveDeadlineSeconds
		out.ActiveDeadlineSeconds = &v
	}
	if in.TTLSecondsAfterFinished != nil {
		v := *in.TTLSecondsAfterFinished
		out.TTLSecondsAfterFinished = &v
	}
}

func (in *PolicyWorkflowRunSpec) DeepCopy() *PolicyWorkflowRunSpec {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowRunSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyWorkflowRunStatus) DeepCopyInto(out *PolicyWorkflowRunStatus) {
	*out = *in
	out.Outputs = copyRawMessageMap(in.Outputs)
	out.Cache = copyPolicyCache(in.Cache)
	out.Annotations = copyRawMessageMap(in.Annotations)
	if in.StartTime != nil {
		out.StartTime = in.StartTime.DeepCopy()
	}
	if in.CompletionTime != nil {
		out.CompletionTime = in.CompletionTime.DeepCopy()
	}
	out.Conditions = copyConditions(in.Conditions)
}

func (in *PolicyWorkflowRunStatus) DeepCopy() *PolicyWorkflowRunStatus {
	if in == nil {
		return nil
	}
	out := new(PolicyWorkflowRunStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyTrigger) DeepCopyInto(out *PolicyTrigger) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PolicyTrigger) DeepCopy() *PolicyTrigger {
	if in == nil {
		return nil
	}
	out := new(PolicyTrigger)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyTrigger) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyTriggerList) DeepCopyInto(out *PolicyTriggerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PolicyTrigger, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PolicyTriggerList) DeepCopy() *PolicyTriggerList {
	if in == nil {
		return nil
	}
	out := new(PolicyTriggerList)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyTriggerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PolicyTriggerSpec) DeepCopyInto(out *PolicyTriggerSpec) {
	*out = *in
	out.Workflow = copyRawMessage(in.Workflow)
	in.Action.DeepCopyInto(&out.Action)
	in.Source.DeepCopyInto(&out.Source)
	out.Match = copyStringMap(in.Match)
	in.RunTemplate.DeepCopyInto(&out.RunTemplate)
}

func (in *PolicyTriggerSpec) DeepCopy() *PolicyTriggerSpec {
	if in == nil {
		return nil
	}
	out := new(PolicyTriggerSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyAction) DeepCopyInto(out *PolicyAction) {
	*out = *in
	out.Spec = copyRawMessage(in.Spec)
}

func (in *PolicyAction) DeepCopy() *PolicyAction {
	if in == nil {
		return nil
	}
	out := new(PolicyAction)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyTriggerSource) DeepCopyInto(out *PolicyTriggerSource) {
	*out = *in
	if in.Record != nil {
		out.Record = in.Record.DeepCopy()
	}
	if in.Relay != nil {
		out.Relay = in.Relay.DeepCopy()
	}
	if in.Cron != nil {
		out.Cron = in.Cron.DeepCopy()
	}
	if in.KCP != nil {
		out.KCP = in.KCP.DeepCopy()
	}
}

func (in *PolicyTriggerSource) DeepCopy() *PolicyTriggerSource {
	if in == nil {
		return nil
	}
	out := new(PolicyTriggerSource)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyRecordTriggerSource) DeepCopyInto(out *PolicyRecordTriggerSource) {
	*out = *in
}

func (in *PolicyRecordTriggerSource) DeepCopy() *PolicyRecordTriggerSource {
	if in == nil {
		return nil
	}
	out := new(PolicyRecordTriggerSource)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyRelayTriggerSource) DeepCopyInto(out *PolicyRelayTriggerSource) {
	*out = *in
}

func (in *PolicyRelayTriggerSource) DeepCopy() *PolicyRelayTriggerSource {
	if in == nil {
		return nil
	}
	out := new(PolicyRelayTriggerSource)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyCronTriggerSource) DeepCopyInto(out *PolicyCronTriggerSource) {
	*out = *in
}

func (in *PolicyCronTriggerSource) DeepCopy() *PolicyCronTriggerSource {
	if in == nil {
		return nil
	}
	out := new(PolicyCronTriggerSource)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyKCPTriggerSource) DeepCopyInto(out *PolicyKCPTriggerSource) {
	*out = *in
}

func (in *PolicyKCPTriggerSource) DeepCopy() *PolicyKCPTriggerSource {
	if in == nil {
		return nil
	}
	out := new(PolicyKCPTriggerSource)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyRunTemplate) DeepCopyInto(out *PolicyRunTemplate) {
	*out = *in
	out.Inputs = copyStringMap(in.Inputs)
	out.Args = copyStringMap(in.Args)
	if in.RFP != nil {
		out.RFP = in.RFP.DeepCopy()
	}
	if in.Bid != nil {
		out.Bid = in.Bid.DeepCopy()
	}
	if in.Accept != nil {
		out.Accept = in.Accept.DeepCopy()
	}
	out.Cache = copyPolicyCache(in.Cache)
	if in.ActiveDeadlineSeconds != nil {
		v := *in.ActiveDeadlineSeconds
		out.ActiveDeadlineSeconds = &v
	}
	if in.TTLSecondsAfterFinished != nil {
		v := *in.TTLSecondsAfterFinished
		out.TTLSecondsAfterFinished = &v
	}
}

func (in *PolicyRunTemplate) DeepCopy() *PolicyRunTemplate {
	if in == nil {
		return nil
	}
	out := new(PolicyRunTemplate)
	in.DeepCopyInto(out)
	return out
}

func (in *PolicyTriggerStatus) DeepCopyInto(out *PolicyTriggerStatus) {
	*out = *in
	if in.LastRun != nil {
		out.LastRun = in.LastRun.DeepCopy()
	}
	if in.NextRun != nil {
		out.NextRun = in.NextRun.DeepCopy()
	}
	out.Conditions = copyConditions(in.Conditions)
}

func (in *PolicyTriggerStatus) DeepCopy() *PolicyTriggerStatus {
	if in == nil {
		return nil
	}
	out := new(PolicyTriggerStatus)
	in.DeepCopyInto(out)
	return out
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyRawMessage(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}

func copyRawMessageMap(in map[string]json.RawMessage) map[string]json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = copyRawMessage(v)
	}
	return out
}

func copyPolicyCache(in PolicyCache) PolicyCache {
	if in == nil {
		return nil
	}
	out := make(PolicyCache, len(in))
	for k := range in {
		out[k] = in[k].DeepCopy()
	}
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

package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (in *Cluster) DeepCopyInto(out *Cluster) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Cluster) DeepCopy() *Cluster {
	if in == nil {
		return nil
	}
	out := new(Cluster)
	in.DeepCopyInto(out)
	return out
}

func (in *Cluster) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ClusterList) DeepCopyInto(out *ClusterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Cluster, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ClusterList) DeepCopy() *ClusterList {
	if in == nil {
		return nil
	}
	out := new(ClusterList)
	in.DeepCopyInto(out)
	return out
}

func (in *ClusterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ClusterStatus) DeepCopyInto(out *ClusterStatus) {
	*out = *in
	if in.HeldPorts != nil {
		out.HeldPorts = make([]ClusterHeldPort, len(in.HeldPorts))
		copy(out.HeldPorts, in.HeldPorts)
	}
	if in.NodeExpiry != nil {
		v := *in.NodeExpiry
		out.NodeExpiry = &v
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *Node) DeepCopyInto(out *Node) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Node) DeepCopy() *Node {
	if in == nil {
		return nil
	}
	out := new(Node)
	in.DeepCopyInto(out)
	return out
}

func (in *Node) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *NodeList) DeepCopyInto(out *NodeList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Node, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *NodeList) DeepCopy() *NodeList {
	if in == nil {
		return nil
	}
	out := new(NodeList)
	in.DeepCopyInto(out)
	return out
}

func (in *NodeList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *NodeStatus) DeepCopyInto(out *NodeStatus) {
	*out = *in
	if in.NodeExpiry != nil {
		v := *in.NodeExpiry
		out.NodeExpiry = &v
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *NodeClaim) DeepCopyInto(out *NodeClaim) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *NodeClaim) DeepCopy() *NodeClaim {
	if in == nil {
		return nil
	}
	out := new(NodeClaim)
	in.DeepCopyInto(out)
	return out
}

func (in *NodeClaim) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *NodeClaimList) DeepCopyInto(out *NodeClaimList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]NodeClaim, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *NodeClaimList) DeepCopy() *NodeClaimList {
	if in == nil {
		return nil
	}
	out := new(NodeClaimList)
	in.DeepCopyInto(out)
	return out
}

func (in *NodeClaimList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *NodeClaimSpec) DeepCopyInto(out *NodeClaimSpec) {
	*out = *in
	if in.DenyBidderDids != nil {
		out.DenyBidderDids = make([]string, len(in.DenyBidderDids))
		copy(out.DenyBidderDids, in.DenyBidderDids)
	}
}

func (in *NodeClaimStatus) DeepCopyInto(out *NodeClaimStatus) {
	*out = *in
	in.Contract.DeepCopyInto(&out.Contract)
	out.Grant = copyPtr(in.Grant)
	out.Secrets = copyPtr(in.Secrets)
}

func (in *NodeClaimContract) DeepCopyInto(out *NodeClaimContract) {
	*out = *in
	out.Version = copyPtr(in.Version)
	out.Payload = copyPtr(in.Payload)
	out.RFP = copyPtr(in.RFP)
	out.Policy = copyPtr(in.Policy)
	out.Bid = copyPtr(in.Bid)
	out.Winner = copyPtr(in.Winner)
	out.AcceptPending = copyPtr(in.AcceptPending)
	out.Accept = copyPtr(in.Accept)
	out.Receipt = copyPtr(in.Receipt)
	out.ReceiptOk = copyPtr(in.ReceiptOk)
	out.Bids = copyPtr(in.Bids)
	if in.Release != nil {
		out.Release = new(ContractRelease)
		in.Release.DeepCopyInto(out.Release)
	}
	if in.Termination != nil {
		out.Termination = new(ContractTermination)
		in.Termination.DeepCopyInto(out.Termination)
	}
	if in.HostKeys != nil {
		out.HostKeys = new(ContractHostKeys)
		in.HostKeys.DeepCopyInto(out.HostKeys)
	}
	out.Timestamps = copyPtr(in.Timestamps)
}

func (in *ContractHostKeys) DeepCopyInto(out *ContractHostKeys) {
	*out = *in
	if in.Keys != nil {
		out.Keys = make([]ContractHostKey, len(in.Keys))
		copy(out.Keys, in.Keys)
	}
}

func (in *ContractTermination) DeepCopyInto(out *ContractTermination) {
	*out = *in
	out.ExitCode = copyPtr(in.ExitCode)
	out.Request = copyPtr(in.Request)
}

func (in *ContractRelease) DeepCopyInto(out *ContractRelease) {
	*out = *in
	out.Status = copyPtr(in.Status)
}

func copyPtr[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out := new(T)
	*out = *in
	return out
}

func (in *AtprotoIdentity) DeepCopyInto(out *AtprotoIdentity) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *AtprotoIdentity) DeepCopy() *AtprotoIdentity {
	if in == nil {
		return nil
	}
	out := new(AtprotoIdentity)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoIdentity) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoIdentityList) DeepCopyInto(out *AtprotoIdentityList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AtprotoIdentity, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *AtprotoIdentityList) DeepCopy() *AtprotoIdentityList {
	if in == nil {
		return nil
	}
	out := new(AtprotoIdentityList)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoIdentityList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoIdentityStatus) DeepCopyInto(out *AtprotoIdentityStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ClusterLink) DeepCopyInto(out *ClusterLink) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

func (in *ClusterLink) DeepCopy() *ClusterLink {
	if in == nil {
		return nil
	}
	out := new(ClusterLink)
	in.DeepCopyInto(out)
	return out
}

func (in *ClusterLink) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ClusterLinkList) DeepCopyInto(out *ClusterLinkList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ClusterLink, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ClusterLinkList) DeepCopy() *ClusterLinkList {
	if in == nil {
		return nil
	}
	out := new(ClusterLinkList)
	in.DeepCopyInto(out)
	return out
}

func (in *ClusterLinkList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketIdentity) DeepCopyInto(out *MarketIdentity) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *MarketIdentity) DeepCopy() *MarketIdentity {
	if in == nil {
		return nil
	}
	out := new(MarketIdentity)
	in.DeepCopyInto(out)
	return out
}

func (in *MarketIdentity) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketIdentityList) DeepCopyInto(out *MarketIdentityList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MarketIdentity, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *MarketIdentityList) DeepCopy() *MarketIdentityList {
	if in == nil {
		return nil
	}
	out := new(MarketIdentityList)
	in.DeepCopyInto(out)
	return out
}

func (in *MarketIdentityList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketIdentitySpec) DeepCopyInto(out *MarketIdentitySpec) {
	*out = *in
	if in.ApprovedAssociations != nil {
		out.ApprovedAssociations = append([]string(nil), in.ApprovedAssociations...)
	}
	if in.RejectedAssociations != nil {
		out.RejectedAssociations = append([]string(nil), in.RejectedAssociations...)
	}
	if in.KeysRef != nil {
		ref := *in.KeysRef
		out.KeysRef = &ref
	}
	if in.SessionRef != nil {
		ref := *in.SessionRef
		out.SessionRef = &ref
	}
}

func (in *MarketIdentityStatus) DeepCopyInto(out *MarketIdentityStatus) {
	*out = *in
	if in.PendingAction != nil {
		action := *in.PendingAction
		out.PendingAction = &action
	}
	if in.Associations != nil {
		out.Associations = append([]string(nil), in.Associations...)
	}
	if in.SessionRef != nil {
		ref := *in.SessionRef
		out.SessionRef = &ref
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ComputeVM) DeepCopyInto(out *ComputeVM) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ComputeVM) DeepCopy() *ComputeVM {
	if in == nil {
		return nil
	}
	out := new(ComputeVM)
	in.DeepCopyInto(out)
	return out
}

func (in *ComputeVM) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ComputeVMList) DeepCopyInto(out *ComputeVMList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ComputeVM, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ComputeVMList) DeepCopy() *ComputeVMList {
	if in == nil {
		return nil
	}
	out := new(ComputeVMList)
	in.DeepCopyInto(out)
	return out
}

func (in *ComputeVMList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ComputeVMSpec) DeepCopyInto(out *ComputeVMSpec) {
	*out = *in
	if in.Contract != nil {
		contract := *in.Contract
		out.Contract = &contract
	}
	if in.Image != nil {
		image := *in.Image
		out.Image = &image
	}
	if in.GuestLogin != nil {
		login := *in.GuestLogin
		out.GuestLogin = &login
	}
}

func (in *ComputeVMStatus) DeepCopyInto(out *ComputeVMStatus) {
	*out = *in
	if in.Network != nil {
		network := *in.Network
		out.Network = &network
	}
	if in.Image != nil {
		image := *in.Image
		out.Image = &image
	}
	if in.Runtime != nil {
		rt := *in.Runtime
		out.Runtime = &rt
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
	if in.Market != nil {
		market := *in.Market
		if in.Market.HostKeys != nil {
			market.HostKeys = append([]ComputeVMHostKey(nil), in.Market.HostKeys...)
		}
		if in.Market.Termination != nil {
			termination := *in.Market.Termination
			if in.Market.Termination.ExitCode != nil {
				code := *in.Market.Termination.ExitCode
				termination.ExitCode = &code
			}
			if in.Market.Termination.Request != nil {
				request := *in.Market.Termination.Request
				termination.Request = &request
			}
			market.Termination = &termination
		}
		out.Market = &market
	}
}

func (in *HostProcess) DeepCopyInto(out *HostProcess) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *HostProcess) DeepCopy() *HostProcess {
	if in == nil {
		return nil
	}
	out := new(HostProcess)
	in.DeepCopyInto(out)
	return out
}

func (in *HostProcess) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostProcessList) DeepCopyInto(out *HostProcessList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]HostProcess, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *HostProcessList) DeepCopy() *HostProcessList {
	if in == nil {
		return nil
	}
	out := new(HostProcessList)
	in.DeepCopyInto(out)
	return out
}

func (in *HostProcessList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostProcessSpec) DeepCopyInto(out *HostProcessSpec) {
	*out = *in
	if in.Asset != nil {
		v := *in.Asset
		out.Asset = &v
	}
	out.Argv = append([]string(nil), in.Argv...)
	out.Env = append([]HostProcessEnv(nil), in.Env...)
	out.EnvFiles = append([]string(nil), in.EnvFiles...)
	if in.Output != nil {
		v := *in.Output
		out.Output = &v
	}
	if in.Limits != nil {
		v := *in.Limits
		out.Limits = &v
	}
	if in.Stop != nil {
		v := *in.Stop
		out.Stop = &v
	}
	out.After = append([]string(nil), in.After...)
	out.Wants = append([]string(nil), in.Wants...)
	if in.Readiness != nil {
		v := *in.Readiness
		v.Exec = append([]string(nil), in.Readiness.Exec...)
		out.Readiness = &v
	}
}

func (in *HostProcessStatus) DeepCopyInto(out *HostProcessStatus) {
	*out = *in
	if in.Runtime != nil {
		v := *in.Runtime
		out.Runtime = &v
	}
	if in.LastResult != nil {
		v := *in.LastResult
		out.LastResult = &v
	}
	out.Listeners = append([]HostProcessListener(nil), in.Listeners...)
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *Postgres) DeepCopyInto(out *Postgres) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Postgres) DeepCopy() *Postgres {
	if in == nil {
		return nil
	}
	out := new(Postgres)
	in.DeepCopyInto(out)
	return out
}

func (in *Postgres) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PostgresList) DeepCopyInto(out *PostgresList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Postgres, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PostgresList) DeepCopy() *PostgresList {
	if in == nil {
		return nil
	}
	out := new(PostgresList)
	in.DeepCopyInto(out)
	return out
}

func (in *PostgresList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PostgresSpec) DeepCopyInto(out *PostgresSpec) {
	*out = *in
	out.Databases = append([]string(nil), in.Databases...)
	out.Settings = append([]string(nil), in.Settings...)
}

func (in *PostgresStatus) DeepCopyInto(out *PostgresStatus) {
	*out = *in
	if in.Runtime != nil {
		v := *in.Runtime
		out.Runtime = &v
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *OpenBaoServer) DeepCopyInto(out *OpenBaoServer) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *OpenBaoServer) DeepCopy() *OpenBaoServer {
	if in == nil {
		return nil
	}
	out := new(OpenBaoServer)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenBaoServer) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *OpenBaoServerList) DeepCopyInto(out *OpenBaoServerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]OpenBaoServer, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *OpenBaoServerList) DeepCopy() *OpenBaoServerList {
	if in == nil {
		return nil
	}
	out := new(OpenBaoServerList)
	in.DeepCopyInto(out)
	return out
}

func (in *OpenBaoServerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *OpenBaoServerSpec) DeepCopyInto(out *OpenBaoServerSpec) {
	b, err := json.Marshal(in)
	if err == nil {
		*out = OpenBaoServerSpec{}
		err = json.Unmarshal(b, out)
	}
	if err != nil {
		*out = *in
	}
}

func (in *OpenBaoServerStatus) DeepCopyInto(out *OpenBaoServerStatus) {
	*out = *in
	if in.Runtime != nil {
		v := *in.Runtime
		out.Runtime = &v
	}
	if in.Refs != nil {
		v := *in.Refs
		out.Refs = &v
	}
	if in.Clients != nil {
		out.Clients = append([]OpenBaoServerClientRefs(nil), in.Clients...)
	}
	if in.Sync != nil {
		v := *in.Sync
		v.Pending = append([]string(nil), in.Sync.Pending...)
		v.Conflicts = append([]string(nil), in.Sync.Conflicts...)
		out.Sync = &v
	}
	if in.Plugins != nil {
		out.Plugins = append([]OpenBaoServerPluginStatus(nil), in.Plugins...)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ComponentRef) DeepCopyInto(out *ComponentRef) {
	*out = *in
}

func (in *ComponentRef) DeepCopy() *ComponentRef {
	if in == nil {
		return nil
	}
	out := new(ComponentRef)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentIdentity) DeepCopyInto(out *ComponentIdentity) {
	*out = *in
	if in.MarketIdentityRef != nil {
		v := *in.MarketIdentityRef
		out.MarketIdentityRef = &v
	}
}

func (in *ComponentIdentity) DeepCopy() *ComponentIdentity {
	if in == nil {
		return nil
	}
	out := new(ComponentIdentity)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentLimits) DeepCopyInto(out *ComponentLimits) {
	*out = *in
}

func (in *ComponentLimits) DeepCopy() *ComponentLimits {
	if in == nil {
		return nil
	}
	out := new(ComponentLimits)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentTLS) DeepCopyInto(out *ComponentTLS) {
	*out = *in
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
}

func (in *ComponentTLS) DeepCopy() *ComponentTLS {
	if in == nil {
		return nil
	}
	out := new(ComponentTLS)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentServiceAuth) DeepCopyInto(out *ComponentServiceAuth) {
	*out = *in
	if in.Audience != nil {
		out.Audience = append([]string(nil), in.Audience...)
	}
}

func (in *ComponentServiceAuth) DeepCopy() *ComponentServiceAuth {
	if in == nil {
		return nil
	}
	out := new(ComponentServiceAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentStorage) DeepCopyInto(out *ComponentStorage) {
	*out = *in
}

func (in *ComponentStorage) DeepCopy() *ComponentStorage {
	if in == nil {
		return nil
	}
	out := new(ComponentStorage)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentServe) DeepCopyInto(out *ComponentServe) {
	*out = *in
}

func (in *ComponentServe) DeepCopy() *ComponentServe {
	if in == nil {
		return nil
	}
	out := new(ComponentServe)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentSpec) DeepCopyInto(out *ComponentSpec) {
	*out = *in
	if in.Network != nil {
		v := *in.Network
		v.RelayUrls = append([]string(nil), in.Network.RelayUrls...)
		v.FirehoseUrls = append([]string(nil), in.Network.FirehoseUrls...)
		v.Hosts = append([]NetworkHost(nil), in.Network.Hosts...)
		out.Network = &v
	}
	if in.Limits != nil {
		v := *in.Limits
		out.Limits = &v
	}
	if in.TLS != nil {
		v := *in.TLS
		v.Hostnames = append([]string(nil), in.TLS.Hostnames...)
		out.TLS = &v
	}
	if in.ServiceAuth != nil {
		v := *in.ServiceAuth
		v.Audience = append([]string(nil), in.ServiceAuth.Audience...)
		out.ServiceAuth = &v
	}
}

func (in *ComponentSpec) DeepCopy() *ComponentSpec {
	if in == nil {
		return nil
	}
	out := new(ComponentSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Endpoint) DeepCopyInto(out *Endpoint) {
	*out = *in
}

func (in *Endpoint) DeepCopy() *Endpoint {
	if in == nil {
		return nil
	}
	out := new(Endpoint)
	in.DeepCopyInto(out)
	return out
}

func (in *ComponentStatus) DeepCopyInto(out *ComponentStatus) {
	*out = *in
	if in.Endpoints != nil {
		out.Endpoints = append([]Endpoint(nil), in.Endpoints...)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ComponentStatus) DeepCopy() *ComponentStatus {
	if in == nil {
		return nil
	}
	out := new(ComponentStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *BidderSpec) DeepCopyInto(out *BidderSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.PdsRef = copyPtr(in.PdsRef)
	out.PDSAccessTokenPath = in.PDSAccessTokenPath
	out.PolicyEngineRef = copyPtr(in.PolicyEngineRef)
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
	if in.Firecracker != nil {
		v := *in.Firecracker
		v.VmruntimeCommand = append([]string(nil), in.Firecracker.VmruntimeCommand...)
		out.Firecracker = &v
	}
	out.Kcp = copyPtr(in.Kcp)
	if in.BidderDIDs != nil {
		out.BidderDIDs = append([]string(nil), in.BidderDIDs...)
	}
}

func (in *BidderSpec) DeepCopy() *BidderSpec {
	if in == nil {
		return nil
	}
	out := new(BidderSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Bidder) DeepCopyInto(out *Bidder) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Bidder) DeepCopy() *Bidder {
	if in == nil {
		return nil
	}
	out := new(Bidder)
	in.DeepCopyInto(out)
	return out
}

func (in *Bidder) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *BidderList) DeepCopyInto(out *BidderList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Bidder, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *BidderList) DeepCopy() *BidderList {
	if in == nil {
		return nil
	}
	out := new(BidderList)
	in.DeepCopyInto(out)
	return out
}

func (in *BidderList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *GuestControlPlaneSpec) DeepCopyInto(out *GuestControlPlaneSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.PdsRef = copyPtr(in.PdsRef)
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *GuestControlPlaneSpec) DeepCopy() *GuestControlPlaneSpec {
	if in == nil {
		return nil
	}
	out := new(GuestControlPlaneSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *GuestControlPlane) DeepCopyInto(out *GuestControlPlane) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *GuestControlPlane) DeepCopy() *GuestControlPlane {
	if in == nil {
		return nil
	}
	out := new(GuestControlPlane)
	in.DeepCopyInto(out)
	return out
}

func (in *GuestControlPlane) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *GuestControlPlaneList) DeepCopyInto(out *GuestControlPlaneList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]GuestControlPlane, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *GuestControlPlaneList) DeepCopy() *GuestControlPlaneList {
	if in == nil {
		return nil
	}
	out := new(GuestControlPlaneList)
	in.DeepCopyInto(out)
	return out
}

func (in *GuestControlPlaneList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PdsSpec) DeepCopyInto(out *PdsSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
	if in.OAuth != nil {
		out.OAuth = new(PdsOAuth)
		in.OAuth.DeepCopyInto(out.OAuth)
	}
}

func (in *PdsSpec) DeepCopy() *PdsSpec {
	if in == nil {
		return nil
	}
	out := new(PdsSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Pds) DeepCopyInto(out *Pds) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Pds) DeepCopy() *Pds {
	if in == nil {
		return nil
	}
	out := new(Pds)
	in.DeepCopyInto(out)
	return out
}

func (in *Pds) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PdsList) DeepCopyInto(out *PdsList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Pds, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PdsList) DeepCopy() *PdsList {
	if in == nil {
		return nil
	}
	out := new(PdsList)
	in.DeepCopyInto(out)
	return out
}

func (in *PdsList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PdsOAuth) DeepCopyInto(out *PdsOAuth) {
	*out = *in
}

func (in *PdsOAuth) DeepCopy() *PdsOAuth {
	if in == nil {
		return nil
	}
	out := new(PdsOAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *RequesterSpec) DeepCopyInto(out *RequesterSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.PdsRef = copyPtr(in.PdsRef)
	out.PDSAccessTokenPath = in.PDSAccessTokenPath
	out.PolicyEngineRef = copyPtr(in.PolicyEngineRef)
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *RequesterSpec) DeepCopy() *RequesterSpec {
	if in == nil {
		return nil
	}
	out := new(RequesterSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Requester) DeepCopyInto(out *Requester) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Requester) DeepCopy() *Requester {
	if in == nil {
		return nil
	}
	out := new(Requester)
	in.DeepCopyInto(out)
	return out
}

func (in *Requester) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RequesterList) DeepCopyInto(out *RequesterList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Requester, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RequesterList) DeepCopy() *RequesterList {
	if in == nil {
		return nil
	}
	out := new(RequesterList)
	in.DeepCopyInto(out)
	return out
}

func (in *RequesterList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *GatewayAPISpec) DeepCopyInto(out *GatewayAPISpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.PdsRef = copyPtr(in.PdsRef)
	out.RequesterRef = copyPtr(in.RequesterRef)
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
	out.ClusterProvider = copyPtr(in.ClusterProvider)
	if in.RequiredDownloads != nil {
		out.RequiredDownloads = append([]string(nil), in.RequiredDownloads...)
	}
	if in.NodeMarket != nil {
		out.NodeMarket = new(GatewayNodeMarket)
		in.NodeMarket.DeepCopyInto(out.NodeMarket)
	}
}

func (in *GatewayNodeMarket) DeepCopyInto(out *GatewayNodeMarket) {
	*out = *in
	if in.ExtraBidderDIDs != nil {
		out.ExtraBidderDIDs = append([]string(nil), in.ExtraBidderDIDs...)
	}
}

func (in *GatewayNodeMarket) DeepCopy() *GatewayNodeMarket {
	if in == nil {
		return nil
	}
	out := new(GatewayNodeMarket)
	in.DeepCopyInto(out)
	return out
}

func (in *GatewayAPISpec) DeepCopy() *GatewayAPISpec {
	if in == nil {
		return nil
	}
	out := new(GatewayAPISpec)
	in.DeepCopyInto(out)
	return out
}

func (in *GatewayAPI) DeepCopyInto(out *GatewayAPI) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *GatewayAPI) DeepCopy() *GatewayAPI {
	if in == nil {
		return nil
	}
	out := new(GatewayAPI)
	in.DeepCopyInto(out)
	return out
}

func (in *GatewayAPI) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *GatewayAPIList) DeepCopyInto(out *GatewayAPIList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]GatewayAPI, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *GatewayAPIList) DeepCopy() *GatewayAPIList {
	if in == nil {
		return nil
	}
	out := new(GatewayAPIList)
	in.DeepCopyInto(out)
	return out
}

func (in *GatewayAPIList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SshServerSpec) DeepCopyInto(out *SshServerSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.RequesterRef = copyPtr(in.RequesterRef)
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *SshServerSpec) DeepCopy() *SshServerSpec {
	if in == nil {
		return nil
	}
	out := new(SshServerSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *SshServer) DeepCopyInto(out *SshServer) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SshServer) DeepCopy() *SshServer {
	if in == nil {
		return nil
	}
	out := new(SshServer)
	in.DeepCopyInto(out)
	return out
}

func (in *SshServer) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SshServerList) DeepCopyInto(out *SshServerList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SshServer, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *SshServerList) DeepCopy() *SshServerList {
	if in == nil {
		return nil
	}
	out := new(SshServerList)
	in.DeepCopyInto(out)
	return out
}

func (in *SshServerList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PlcSpec) DeepCopyInto(out *PlcSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.VaultRef = copyPtr(in.VaultRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *PlcSpec) DeepCopy() *PlcSpec {
	if in == nil {
		return nil
	}
	out := new(PlcSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Plc) DeepCopyInto(out *Plc) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Plc) DeepCopy() *Plc {
	if in == nil {
		return nil
	}
	out := new(Plc)
	in.DeepCopyInto(out)
	return out
}

func (in *Plc) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PlcList) DeepCopyInto(out *PlcList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Plc, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PlcList) DeepCopy() *PlcList {
	if in == nil {
		return nil
	}
	out := new(PlcList)
	in.DeepCopyInto(out)
	return out
}

func (in *PlcList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RelaySpec) DeepCopyInto(out *RelaySpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.VaultRef = copyPtr(in.VaultRef)
	if in.PdsRefs != nil {
		out.PdsRefs = append([]ComponentRef(nil), in.PdsRefs...)
	}
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *RelaySpec) DeepCopy() *RelaySpec {
	if in == nil {
		return nil
	}
	out := new(RelaySpec)
	in.DeepCopyInto(out)
	return out
}

func (in *Relay) DeepCopyInto(out *Relay) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *Relay) DeepCopy() *Relay {
	if in == nil {
		return nil
	}
	out := new(Relay)
	in.DeepCopyInto(out)
	return out
}

func (in *Relay) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RelayList) DeepCopyInto(out *RelayList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Relay, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RelayList) DeepCopy() *RelayList {
	if in == nil {
		return nil
	}
	out := new(RelayList)
	in.DeepCopyInto(out)
	return out
}

func (in *RelayList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *IngressProxySpec) DeepCopyInto(out *IngressProxySpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	if in.Identity != nil {
		out.Identity = new(ComponentIdentity)
		in.Identity.DeepCopyInto(out.Identity)
	}
	out.VaultRef = copyPtr(in.VaultRef)
	out.RelayRef = copyPtr(in.RelayRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
	out.Iroh = copyPtr(in.Iroh)
	if in.OpenTunnel != nil {
		out.OpenTunnel = new(IngressProxyOpenTunnel)
		in.OpenTunnel.DeepCopyInto(out.OpenTunnel)
	}
}

func (in *IngressProxyIroh) DeepCopyInto(out *IngressProxyIroh) {
	*out = *in
}

func (in *IngressProxyIroh) DeepCopy() *IngressProxyIroh {
	if in == nil {
		return nil
	}
	out := new(IngressProxyIroh)
	in.DeepCopyInto(out)
	return out
}

func (in *IngressProxyOpenTunnel) DeepCopyInto(out *IngressProxyOpenTunnel) {
	*out = *in
	if in.Command != nil {
		out.Command = append([]string(nil), in.Command...)
	}
}

func (in *IngressProxyOpenTunnel) DeepCopy() *IngressProxyOpenTunnel {
	if in == nil {
		return nil
	}
	out := new(IngressProxyOpenTunnel)
	in.DeepCopyInto(out)
	return out
}

func (in *IngressProxySpec) DeepCopy() *IngressProxySpec {
	if in == nil {
		return nil
	}
	out := new(IngressProxySpec)
	in.DeepCopyInto(out)
	return out
}

func (in *IngressProxy) DeepCopyInto(out *IngressProxy) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *IngressProxy) DeepCopy() *IngressProxy {
	if in == nil {
		return nil
	}
	out := new(IngressProxy)
	in.DeepCopyInto(out)
	return out
}

func (in *IngressProxy) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *IngressProxyList) DeepCopyInto(out *IngressProxyList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]IngressProxy, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *IngressProxyList) DeepCopy() *IngressProxyList {
	if in == nil {
		return nil
	}
	out := new(IngressProxyList)
	in.DeepCopyInto(out)
	return out
}

func (in *IngressProxyList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DispatcherTLSSpec) DeepCopyInto(out *DispatcherTLSSpec) {
	*out = *in
	in.ComponentSpec.DeepCopyInto(&out.ComponentSpec)
	out.VaultRef = copyPtr(in.VaultRef)
	out.UpstreamRef = copyPtr(in.UpstreamRef)
	if in.Hostnames != nil {
		out.Hostnames = append([]string(nil), in.Hostnames...)
	}
	out.Storage = copyPtr(in.Storage)
	out.Serve = copyPtr(in.Serve)
}

func (in *DispatcherTLSSpec) DeepCopy() *DispatcherTLSSpec {
	if in == nil {
		return nil
	}
	out := new(DispatcherTLSSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *DispatcherTLS) DeepCopyInto(out *DispatcherTLS) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *DispatcherTLS) DeepCopy() *DispatcherTLS {
	if in == nil {
		return nil
	}
	out := new(DispatcherTLS)
	in.DeepCopyInto(out)
	return out
}

func (in *DispatcherTLS) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *DispatcherTLSList) DeepCopyInto(out *DispatcherTLSList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]DispatcherTLS, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *DispatcherTLSList) DeepCopy() *DispatcherTLSList {
	if in == nil {
		return nil
	}
	out := new(DispatcherTLSList)
	in.DeepCopyInto(out)
	return out
}

func (in *DispatcherTLSList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketPresence) DeepCopyInto(out *MarketPresence) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *MarketPresence) DeepCopy() *MarketPresence {
	if in == nil {
		return nil
	}
	out := new(MarketPresence)
	in.DeepCopyInto(out)
	return out
}

func (in *MarketPresence) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketPresenceList) DeepCopyInto(out *MarketPresenceList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]MarketPresence, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *MarketPresenceList) DeepCopy() *MarketPresenceList {
	if in == nil {
		return nil
	}
	out := new(MarketPresenceList)
	in.DeepCopyInto(out)
	return out
}

func (in *MarketPresenceList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *MarketPresenceSpec) DeepCopyInto(out *MarketPresenceSpec) {
	*out = *in
	out.BidderRef = in.BidderRef
}

func (in *MarketPresenceStatus) DeepCopyInto(out *MarketPresenceStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ChildApplication) DeepCopyInto(out *ChildApplication) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ChildApplication) DeepCopy() *ChildApplication {
	if in == nil {
		return nil
	}
	out := new(ChildApplication)
	in.DeepCopyInto(out)
	return out
}

func (in *ChildApplication) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ChildApplicationList) DeepCopyInto(out *ChildApplicationList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ChildApplication, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ChildApplicationList) DeepCopy() *ChildApplicationList {
	if in == nil {
		return nil
	}
	out := new(ChildApplicationList)
	in.DeepCopyInto(out)
	return out
}

func (in *ChildApplicationList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ChildApplicationSpec) DeepCopyInto(out *ChildApplicationSpec) {
	*out = *in
}

func (in *ChildApplicationComponent) DeepCopyInto(out *ChildApplicationComponent) {
	*out = *in
}

func (in *ChildApplicationStatus) DeepCopyInto(out *ChildApplicationStatus) {
	*out = *in
	if in.Components != nil {
		out.Components = make([]ChildApplicationComponent, len(in.Components))
		copy(out.Components, in.Components)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *HostInstall) DeepCopyInto(out *HostInstall) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *HostInstall) DeepCopy() *HostInstall {
	if in == nil {
		return nil
	}
	out := new(HostInstall)
	in.DeepCopyInto(out)
	return out
}

func (in *HostInstall) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostInstallList) DeepCopyInto(out *HostInstallList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]HostInstall, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *HostInstallList) DeepCopy() *HostInstallList {
	if in == nil {
		return nil
	}
	out := new(HostInstallList)
	in.DeepCopyInto(out)
	return out
}

func (in *HostInstallList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostInstallSpec) DeepCopyInto(out *HostInstallSpec) {
	*out = *in
}

func (in *HostInstallStep) DeepCopyInto(out *HostInstallStep) {
	*out = *in
}

func (in *HostInstallStatus) DeepCopyInto(out *HostInstallStatus) {
	*out = *in
	if in.Steps != nil {
		out.Steps = make([]HostInstallStep, len(in.Steps))
		copy(out.Steps, in.Steps)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *HostRelease) DeepCopyInto(out *HostRelease) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *HostRelease) DeepCopy() *HostRelease {
	if in == nil {
		return nil
	}
	out := new(HostRelease)
	in.DeepCopyInto(out)
	return out
}

func (in *HostRelease) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostReleaseList) DeepCopyInto(out *HostReleaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]HostRelease, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *HostReleaseList) DeepCopy() *HostReleaseList {
	if in == nil {
		return nil
	}
	out := new(HostReleaseList)
	in.DeepCopyInto(out)
	return out
}

func (in *HostReleaseList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *HostReleaseSpec) DeepCopyInto(out *HostReleaseSpec) {
	*out = *in
}

func (in *HostReleaseSelfCheck) DeepCopyInto(out *HostReleaseSelfCheck) {
	*out = *in
}

func (in *HostReleaseGC) DeepCopyInto(out *HostReleaseGC) {
	*out = *in
	if in.Removed != nil {
		out.Removed = make([]string, len(in.Removed))
		copy(out.Removed, in.Removed)
	}
}

func (in *HostReleaseStatus) DeepCopyInto(out *HostReleaseStatus) {
	*out = *in
	if in.Previous != nil {
		out.Previous = make([]string, len(in.Previous))
		copy(out.Previous, in.Previous)
	}
	if in.SelfCheck != nil {
		v := *in.SelfCheck
		out.SelfCheck = &v
	}
	if in.GC != nil {
		out.GC = new(HostReleaseGC)
		in.GC.DeepCopyInto(out.GC)
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *KcpInstallation) DeepCopyInto(out *KcpInstallation) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *KcpInstallation) DeepCopy() *KcpInstallation {
	if in == nil {
		return nil
	}
	out := new(KcpInstallation)
	in.DeepCopyInto(out)
	return out
}

func (in *KcpInstallation) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *KcpInstallationList) DeepCopyInto(out *KcpInstallationList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KcpInstallation, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KcpInstallationList) DeepCopy() *KcpInstallationList {
	if in == nil {
		return nil
	}
	out := new(KcpInstallationList)
	in.DeepCopyInto(out)
	return out
}

func (in *KcpInstallationList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *KcpInstallationSpec) DeepCopyInto(out *KcpInstallationSpec) {
	*out = *in
}

func (in *KcpInstallationStatus) DeepCopyInto(out *KcpInstallationStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *AtprotoRepo) DeepCopyInto(out *AtprotoRepo) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *AtprotoRepo) DeepCopy() *AtprotoRepo {
	if in == nil {
		return nil
	}
	out := new(AtprotoRepo)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoRepo) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoRepoList) DeepCopyInto(out *AtprotoRepoList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AtprotoRepo, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *AtprotoRepoList) DeepCopy() *AtprotoRepoList {
	if in == nil {
		return nil
	}
	out := new(AtprotoRepoList)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoRepoList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoRepoStatus) DeepCopyInto(out *AtprotoRepoStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *PdsAccount) DeepCopyInto(out *PdsAccount) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PdsAccount) DeepCopy() *PdsAccount {
	if in == nil {
		return nil
	}
	out := new(PdsAccount)
	in.DeepCopyInto(out)
	return out
}

func (in *PdsAccount) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PdsAccountList) DeepCopyInto(out *PdsAccountList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PdsAccount, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PdsAccountList) DeepCopy() *PdsAccountList {
	if in == nil {
		return nil
	}
	out := new(PdsAccountList)
	in.DeepCopyInto(out)
	return out
}

func (in *PdsAccountList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PdsAccountStatus) DeepCopyInto(out *PdsAccountStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *AtprotoSession) DeepCopyInto(out *AtprotoSession) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *AtprotoSession) DeepCopy() *AtprotoSession {
	if in == nil {
		return nil
	}
	out := new(AtprotoSession)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoSession) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoSessionList) DeepCopyInto(out *AtprotoSessionList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AtprotoSession, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *AtprotoSessionList) DeepCopy() *AtprotoSessionList {
	if in == nil {
		return nil
	}
	out := new(AtprotoSessionList)
	in.DeepCopyInto(out)
	return out
}

func (in *AtprotoSessionList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *AtprotoSessionStatus) DeepCopyInto(out *AtprotoSessionStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *PlcIdentity) DeepCopyInto(out *PlcIdentity) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PlcIdentity) DeepCopy() *PlcIdentity {
	if in == nil {
		return nil
	}
	out := new(PlcIdentity)
	in.DeepCopyInto(out)
	return out
}

func (in *PlcIdentity) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PlcIdentityList) DeepCopyInto(out *PlcIdentityList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PlcIdentity, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PlcIdentityList) DeepCopy() *PlcIdentityList {
	if in == nil {
		return nil
	}
	out := new(PlcIdentityList)
	in.DeepCopyInto(out)
	return out
}

func (in *PlcIdentityList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PlcIdentityStatus) DeepCopyInto(out *PlcIdentityStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *PortLease) DeepCopyInto(out *PortLease) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *PortLease) DeepCopy() *PortLease {
	if in == nil {
		return nil
	}
	out := new(PortLease)
	in.DeepCopyInto(out)
	return out
}

func (in *PortLease) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PortLeaseList) DeepCopyInto(out *PortLeaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]PortLease, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *PortLeaseList) DeepCopy() *PortLeaseList {
	if in == nil {
		return nil
	}
	out := new(PortLeaseList)
	in.DeepCopyInto(out)
	return out
}

func (in *PortLeaseList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *PortLeaseStatus) DeepCopyInto(out *PortLeaseStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *SubnetLease) DeepCopyInto(out *SubnetLease) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *SubnetLease) DeepCopy() *SubnetLease {
	if in == nil {
		return nil
	}
	out := new(SubnetLease)
	in.DeepCopyInto(out)
	return out
}

func (in *SubnetLease) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SubnetLeaseList) DeepCopyInto(out *SubnetLeaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]SubnetLease, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *SubnetLeaseList) DeepCopy() *SubnetLeaseList {
	if in == nil {
		return nil
	}
	out := new(SubnetLeaseList)
	in.DeepCopyInto(out)
	return out
}

func (in *SubnetLeaseList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *SubnetLeaseStatus) DeepCopyInto(out *SubnetLeaseStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *ClusterRoll) DeepCopyInto(out *ClusterRoll) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *ClusterRoll) DeepCopy() *ClusterRoll {
	if in == nil {
		return nil
	}
	out := new(ClusterRoll)
	in.DeepCopyInto(out)
	return out
}

func (in *ClusterRoll) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ClusterRollList) DeepCopyInto(out *ClusterRollList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ClusterRoll, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *ClusterRollList) DeepCopy() *ClusterRollList {
	if in == nil {
		return nil
	}
	out := new(ClusterRollList)
	in.DeepCopyInto(out)
	return out
}

func (in *ClusterRollList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *ClusterRollStatus) DeepCopyInto(out *ClusterRollStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *RelayStatus) DeepCopyInto(out *RelayStatus) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *RelayStatus) DeepCopy() *RelayStatus {
	if in == nil {
		return nil
	}
	out := new(RelayStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *RelayStatus) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RelayStatusList) DeepCopyInto(out *RelayStatusList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]RelayStatus, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RelayStatusList) DeepCopy() *RelayStatusList {
	if in == nil {
		return nil
	}
	out := new(RelayStatusList)
	in.DeepCopyInto(out)
	return out
}

func (in *RelayStatusList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RelayStatusStatus) DeepCopyInto(out *RelayStatusStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *VaultCredential) DeepCopyInto(out *VaultCredential) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *VaultCredential) DeepCopy() *VaultCredential {
	if in == nil {
		return nil
	}
	out := new(VaultCredential)
	in.DeepCopyInto(out)
	return out
}

func (in *VaultCredential) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *VaultCredentialList) DeepCopyInto(out *VaultCredentialList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]VaultCredential, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *VaultCredentialList) DeepCopy() *VaultCredentialList {
	if in == nil {
		return nil
	}
	out := new(VaultCredentialList)
	in.DeepCopyInto(out)
	return out
}

func (in *VaultCredentialList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *VaultCredentialStatus) DeepCopyInto(out *VaultCredentialStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *VaultCertificate) DeepCopyInto(out *VaultCertificate) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

func (in *VaultCertificate) DeepCopy() *VaultCertificate {
	if in == nil {
		return nil
	}
	out := new(VaultCertificate)
	in.DeepCopyInto(out)
	return out
}

func (in *VaultCertificate) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *VaultCertificateList) DeepCopyInto(out *VaultCertificateList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]VaultCertificate, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *VaultCertificateList) DeepCopy() *VaultCertificateList {
	if in == nil {
		return nil
	}
	out := new(VaultCertificateList)
	in.DeepCopyInto(out)
	return out
}

func (in *VaultCertificateList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *VaultCertificateStatus) DeepCopyInto(out *VaultCertificateStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *StateSync) DeepCopyInto(out *StateSync) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *StateSync) DeepCopy() *StateSync {
	if in == nil {
		return nil
	}
	out := new(StateSync)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSync) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *StateSyncList) DeepCopyInto(out *StateSyncList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]StateSync, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *StateSyncList) DeepCopy() *StateSyncList {
	if in == nil {
		return nil
	}
	out := new(StateSyncList)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *StateSyncSource) DeepCopyInto(out *StateSyncSource) {
	*out = *in
}

func (in *StateSyncSource) DeepCopy() *StateSyncSource {
	if in == nil {
		return nil
	}
	out := new(StateSyncSource)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncSink) DeepCopyInto(out *StateSyncSink) {
	*out = *in
}

func (in *StateSyncSink) DeepCopy() *StateSyncSink {
	if in == nil {
		return nil
	}
	out := new(StateSyncSink)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncAck) DeepCopyInto(out *StateSyncAck) {
	*out = *in
	if in.Paths != nil {
		out.Paths = make([]string, len(in.Paths))
		copy(out.Paths, in.Paths)
	}
}

func (in *StateSyncAck) DeepCopy() *StateSyncAck {
	if in == nil {
		return nil
	}
	out := new(StateSyncAck)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncRetry) DeepCopyInto(out *StateSyncRetry) {
	*out = *in
}

func (in *StateSyncRetry) DeepCopy() *StateSyncRetry {
	if in == nil {
		return nil
	}
	out := new(StateSyncRetry)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncSpec) DeepCopyInto(out *StateSyncSpec) {
	*out = *in
	if in.Source != nil {
		in, out := &in.Source, &out.Source
		*out = new(StateSyncSource)
		**out = **in
	}
	out.Sink = in.Sink
	if in.Payload != nil {
		out.Payload = append(json.RawMessage(nil), in.Payload...)
	}
	if in.Ack != nil {
		in, out := &in.Ack, &out.Ack
		*out = new(StateSyncAck)
		(*in).DeepCopyInto(*out)
	}
	if in.Retry != nil {
		in, out := &in.Retry, &out.Retry
		*out = new(StateSyncRetry)
		**out = **in
	}
}

func (in *StateSyncSpec) DeepCopy() *StateSyncSpec {
	if in == nil {
		return nil
	}
	out := new(StateSyncSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncObservation) DeepCopyInto(out *StateSyncObservation) {
	*out = *in
	if in.Meta != nil {
		out.Meta = make(map[string]string, len(in.Meta))
		for key, value := range in.Meta {
			out.Meta[key] = value
		}
	}
}

func (in *StateSyncObservation) DeepCopy() *StateSyncObservation {
	if in == nil {
		return nil
	}
	out := new(StateSyncObservation)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncAcknowledged) DeepCopyInto(out *StateSyncAcknowledged) {
	*out = *in
}

func (in *StateSyncAcknowledged) DeepCopy() *StateSyncAcknowledged {
	if in == nil {
		return nil
	}
	out := new(StateSyncAcknowledged)
	in.DeepCopyInto(out)
	return out
}

func (in *StateSyncStatus) DeepCopyInto(out *StateSyncStatus) {
	*out = *in
	if in.Observed != nil {
		in, out := &in.Observed, &out.Observed
		*out = new(StateSyncObservation)
		(*in).DeepCopyInto(*out)
	}
	if in.Acked != nil {
		in, out := &in.Acked, &out.Acked
		*out = new(StateSyncAcknowledged)
		**out = **in
	}
	if in.Cursor != nil {
		in, out := &in.Cursor, &out.Cursor
		*out = new(int64)
		**out = **in
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *StateSyncStatus) DeepCopy() *StateSyncStatus {
	if in == nil {
		return nil
	}
	out := new(StateSyncStatus)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBao) DeepCopyInto(out *RemoteOpenBao) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *RemoteOpenBao) DeepCopy() *RemoteOpenBao {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBao)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBao) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RemoteOpenBaoList) DeepCopyInto(out *RemoteOpenBaoList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]RemoteOpenBao, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *RemoteOpenBaoList) DeepCopy() *RemoteOpenBaoList {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoList)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

func (in *RemoteOpenBaoSpec) DeepCopyInto(out *RemoteOpenBaoSpec) {
	*out = *in
	out.TLS = in.TLS
	in.Auth.DeepCopyInto(&out.Auth)
	out.Mounts = in.Mounts
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
}

func (in *RemoteOpenBaoSpec) DeepCopy() *RemoteOpenBaoSpec {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoSpec)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoTLS) DeepCopyInto(out *RemoteOpenBaoTLS) {
	*out = *in
}

func (in *RemoteOpenBaoTLS) DeepCopy() *RemoteOpenBaoTLS {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoTLS)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoAuth) DeepCopyInto(out *RemoteOpenBaoAuth) {
	*out = *in
	if in.AppRole != nil {
		in, out := &in.AppRole, &out.AppRole
		*out = new(RemoteOpenBaoAppRole)
		**out = **in
	}
	if in.Cert != nil {
		in, out := &in.Cert, &out.Cert
		*out = new(RemoteOpenBaoCert)
		**out = **in
	}
}

func (in *RemoteOpenBaoAuth) DeepCopy() *RemoteOpenBaoAuth {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoAuth)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoAppRole) DeepCopyInto(out *RemoteOpenBaoAppRole) {
	*out = *in
}

func (in *RemoteOpenBaoAppRole) DeepCopy() *RemoteOpenBaoAppRole {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoAppRole)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoCert) DeepCopyInto(out *RemoteOpenBaoCert) {
	*out = *in
}

func (in *RemoteOpenBaoCert) DeepCopy() *RemoteOpenBaoCert {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoCert)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoMounts) DeepCopyInto(out *RemoteOpenBaoMounts) {
	*out = *in
}

func (in *RemoteOpenBaoMounts) DeepCopy() *RemoteOpenBaoMounts {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoMounts)
	in.DeepCopyInto(out)
	return out
}

func (in *RemoteOpenBaoStatus) DeepCopyInto(out *RemoteOpenBaoStatus) {
	*out = *in
	if in.LastCheckedAt != nil {
		out.LastCheckedAt = in.LastCheckedAt.DeepCopy()
	}
	if in.Conditions != nil {
		out.Conditions = make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *RemoteOpenBaoStatus) DeepCopy() *RemoteOpenBaoStatus {
	if in == nil {
		return nil
	}
	out := new(RemoteOpenBaoStatus)
	in.DeepCopyInto(out)
	return out
}

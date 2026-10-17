package v1alpha1

//go:generate go run ../../cmd/socialweb-apigen -root ../..

type PrinterColumn struct {
	Name string

	Type string

	JSONPath string
}

type APIResource struct {
	Kind string

	Plural string

	Revision int

	Export string

	Object any

	PrinterColumns []PrinterColumn
}

var APIResources = []APIResource{
	{Kind: "Cluster", Plural: "clusters", Revision: 1, Export: "clusters", Object: Cluster{}},
	{Kind: "Node", Plural: "nodes", Revision: 1, Export: "clusters", Object: Node{}},
	{Kind: "NodeClaim", Plural: "nodeclaims", Revision: 5, Export: "clusters", Object: NodeClaim{}},
	{Kind: "ClusterLink", Plural: "clusterlinks", Revision: 1, Export: "clusters", Object: ClusterLink{}},
	{Kind: "ClusterClientCertificate", Plural: "clusterclientcertificates", Revision: 1, Export: "clusters", Object: ClusterClientCertificate{}, PrinterColumns: []PrinterColumn{
		{Name: "Cluster", Type: "string", JSONPath: ".spec.cluster"},
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "Serial", Type: "string", JSONPath: ".spec.serial"},
		{Name: "NotAfter", Type: "string", JSONPath: ".spec.notAfter"},
		{Name: "NoExpiry", Type: "boolean", JSONPath: ".spec.noExpiry"},
		{Name: "Revoked", Type: "boolean", JSONPath: ".spec.revoked"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoIdentity", Plural: "atprotoidentities", Revision: 1, Export: "atprotoidentities", Object: AtprotoIdentity{}},
	{Kind: "MarketIdentity", Plural: "marketidentities", Revision: 2, Export: "marketidentities", Object: MarketIdentity{}},
	{Kind: "AtprotoRecord", Plural: "atprotorecords", Revision: 2, Export: "marketidentities", Object: AtprotoRecord{}, PrinterColumns: []PrinterColumn{
		{Name: "Repo", Type: "string", JSONPath: ".spec.repo"},
		{Name: "Collection", Type: "string", JSONPath: ".spec.collection"},
		{Name: "Rkey", Type: "string", JSONPath: ".spec.rkey"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoRecordWatch", Plural: "atprotorecordwatches", Revision: 1, Export: "marketidentities", Object: AtprotoRecordWatch{}, PrinterColumns: []PrinterColumn{
		{Name: "Repo", Type: "string", JSONPath: ".spec.repo"},
		{Name: "Collection", Type: "string", JSONPath: ".spec.collection"},
		{Name: "Message", Type: "string", JSONPath: ".status.message"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ComputeVM", Plural: "computevms", Revision: 3, Export: "computevms", Object: ComputeVM{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "GuestIP", Type: "string", JSONPath: ".status.network.guestIP"},
		{Name: "Requester", Type: "string", JSONPath: ".spec.requester.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Bid", Plural: "bids", Revision: 4, Export: "computevms", Object: Bid{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Requester", Type: "string", JSONPath: ".spec.requester"},
		{Name: "Verdict", Type: "string", JSONPath: ".spec.policy.verdict"},
		{Name: "Published", Type: "string", JSONPath: ".status.timings.publishedAt"},
		{Name: "ComputeVM", Type: "string", JSONPath: ".status.computeVM"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "GuestIdentityGrant", Plural: "guestidentitygrants", Revision: 1, Export: "computevms", Object: GuestIdentityGrant{}, PrinterColumns: []PrinterColumn{
		{Name: "ComputeVM", Type: "string", JSONPath: ".spec.computevm"},
		{Name: "API", Type: "string", JSONPath: ".spec.api"},
		{Name: "Actx", Type: "string", JSONPath: ".spec.actx"},
		{Name: "Applied", Type: "boolean", JSONPath: ".status.applied"},
		{Name: "Role", Type: "string", JSONPath: ".status.role"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "HostProcess", Plural: "hostprocesses", Revision: 3, Export: "hostprocesses", Object: HostProcess{}, PrinterColumns: []PrinterColumn{
		{Name: "Placement", Type: "string", JSONPath: ".spec.placement"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Backend", Type: "string", JSONPath: ".status.runtime.backend"},
		{Name: "Unit", Type: "string", JSONPath: ".spec.unit"},
		{Name: "PID", Type: "integer", JSONPath: ".status.runtime.pid"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Postgres", Plural: "postgreses", Revision: 1, Export: "hostprocesses", Object: Postgres{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Version", Type: "string", JSONPath: ".status.version"},
		{Name: "Socket", Type: "string", JSONPath: ".status.socketDir"},
		{Name: "PID", Type: "integer", JSONPath: ".status.runtime.pid"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "OpenBaoServer", Plural: "openbaoservers", Revision: 10, Export: "hostprocesses", Object: OpenBaoServer{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Address", Type: "string", JSONPath: ".status.address"},
		{Name: "Initialized", Type: "boolean", JSONPath: ".status.initialized"},
		{Name: "Sealed", Type: "boolean", JSONPath: ".status.sealed"},
		{Name: "Instance", Type: "string", JSONPath: ".status.runtime.instance"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketNetwork", Plural: "marketnetworks", Revision: 9, Export: "applications", Object: MarketNetwork{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "PLC", Type: "string", JSONPath: ".status.network.plcDirectoryUrl"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketBidder", Plural: "marketbidders", Revision: 12, Export: "applications", Object: MarketBidder{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "DID", Type: "string", JSONPath: ".status.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketRequester", Plural: "marketrequesters", Revision: 9, Export: "applications", Object: MarketRequester{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "DID", Type: "string", JSONPath: ".status.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ComputeContract", Plural: "computecontracts", Revision: 4, Export: "computecontracts", Object: ComputeContract{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Contract", Type: "string", JSONPath: ".status.contract.phase"},
		{Name: "Winner", Type: "string", JSONPath: ".status.contract.winner.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Gateway", Plural: "gateways", Revision: 13, Export: "applications", Object: Gateway{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "SshGateway", Plural: "sshgateways", Revision: 7, Export: "applications", Object: SshGateway{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}

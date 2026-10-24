package v1alpha1

//go:generate bash ../../hack/codegen.sh
//go:generate go run ../../cmd/socialweb-apigen -root ../..

import (
	"github.com/johnandersen777/socialweb-computer-kcp/api/apiresource"
	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PrinterColumn = apiresource.PrinterColumn

type APIResource = apiresource.APIResource

var APIResources = []APIResource{
	{Kind: "Cluster", Plural: "clusters", Revision: 3, Export: "clusters", Object: Cluster{}},
	{Kind: "Node", Plural: "nodes", Revision: 2, Export: "clusters", Object: Node{}},
	{Kind: "NodeClaim", Plural: "nodeclaims", Revision: 8, Export: "clusters", Object: NodeClaim{}},
	{Kind: "ClusterLink", Plural: "clusterlinks", Revision: 2, Export: "clusters", Object: ClusterLink{}},
	{Kind: "ClusterClientCertificate", Plural: "clusterclientcertificates", Revision: 2, Export: "clusters", Object: ClusterClientCertificate{}, PrinterColumns: []PrinterColumn{
		{Name: "Cluster", Type: "string", JSONPath: ".spec.clusterRef.name"},
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "Serial", Type: "string", JSONPath: ".spec.serial"},
		{Name: "NotAfter", Type: "string", JSONPath: ".spec.notAfter"},
		{Name: "NoExpiry", Type: "boolean", JSONPath: ".spec.noExpiry"},
		{Name: "Revoked", Type: "boolean", JSONPath: ".spec.revoked"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoIdentity", Plural: "atprotoidentities", Revision: 2, Export: "atprotoidentities", Object: AtprotoIdentity{}},
	{Kind: "MarketIdentity", Plural: "marketidentities", Revision: 8, Export: "marketidentities", Object: MarketIdentity{}},
	{Kind: "AtprotoRecord", Plural: "atprotorecords", Revision: 5, Export: "marketidentities", Object: AtprotoRecord{}, PrinterColumns: []PrinterColumn{
		{Name: "Repo", Type: "string", JSONPath: ".spec.repoRef.name"},
		{Name: "Collection", Type: "string", JSONPath: ".spec.collection"},
		{Name: "Rkey", Type: "string", JSONPath: ".spec.rkey"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoRecordWatch", Plural: "atprotorecordwatches", Revision: 5, Export: "marketidentities", Object: AtprotoRecordWatch{}, PrinterColumns: []PrinterColumn{
		{Name: "Repo", Type: "string", JSONPath: ".spec.repo"},
		{Name: "Collection", Type: "string", JSONPath: ".spec.collection"},
		{Name: "Message", Type: "string", JSONPath: ".status.message"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ComputeVM", Plural: "computevms", Revision: 8, Export: "computevms", Object: ComputeVM{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Backend", Type: "string", JSONPath: ".spec.backendRef.kind"},
		{Name: "GuestIP", Type: "string", JSONPath: ".status.addresses.guest"},
		{Name: "Requester", Type: "string", JSONPath: ".spec.requester.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "FirecrackerVM", Plural: "firecrackervms", Revision: 2, Export: "computevms", Object: FirecrackerVM{}, Contracts: []string{commonv1.ContractComputeVMs}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "GuestIP", Type: "string", JSONPath: ".status.network.guestIP"},
		{Name: "Unit", Type: "string", JSONPath: ".status.runtime.unit"},
		{Name: "PID", Type: "integer", JSONPath: ".status.runtime.pid"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Bid", Plural: "bids", Revision: 8, Export: "computevms", Object: Bid{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Requester", Type: "string", JSONPath: ".spec.requester"},
		{Name: "Verdict", Type: "string", JSONPath: ".spec.policy.verdict"},
		{Name: "Published", Type: "string", JSONPath: ".status.timings.publishedAt"},
		{Name: "ComputeVM", Type: "string", JSONPath: ".status.computeVM"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "GuestIdentityGrant", Plural: "guestidentitygrants", Revision: 3, Export: "computevms", Object: GuestIdentityGrant{}, PrinterColumns: []PrinterColumn{
		{Name: "ComputeVM", Type: "string", JSONPath: ".spec.computeVMRef.name"},
		{Name: "API", Type: "string", JSONPath: ".spec.api"},
		{Name: "Actx", Type: "string", JSONPath: ".spec.actx"},
		{Name: "Applied", Type: "boolean", JSONPath: ".status.applied"},
		{Name: "Role", Type: "string", JSONPath: ".status.role"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "HostProcess", Plural: "hostprocesses", Revision: 4, Export: "hostprocesses", Object: HostProcess{}, PrinterColumns: []PrinterColumn{
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
	{Kind: "OpenBaoServer", Plural: "openbaoservers", Revision: 11, Export: "hostprocesses", Object: OpenBaoServer{}, Contracts: []string{commonv1.ContractVaults}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Address", Type: "string", JSONPath: ".status.address"},
		{Name: "Initialized", Type: "boolean", JSONPath: ".status.initialized"},
		{Name: "Sealed", Type: "boolean", JSONPath: ".status.sealed"},
		{Name: "Instance", Type: "string", JSONPath: ".status.runtime.instance"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "RemoteOpenBao", Plural: "remoteopenbaos", Revision: 3, Export: "hostprocesses", Object: RemoteOpenBao{}, Contracts: []string{commonv1.ContractVaults}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Endpoint", Type: "string", JSONPath: ".status.endpoint"},
		{Name: "Sealed", Type: "boolean", JSONPath: ".status.sealed"},
	}},
	{Kind: "Vault", Plural: "vaults", Revision: 1, Export: "hostprocesses", Object: Vault{}, PrinterColumns: []PrinterColumn{
		{Name: "Backend", Type: "string", JSONPath: ".spec.backendRef.kind"},
		{Name: "Address", Type: "string", JSONPath: ".status.address"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Sealed", Type: "boolean", JSONPath: ".status.sealed"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketNetwork", Plural: "marketnetworks", Revision: 20, Export: "applications", Object: MarketNetwork{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "PLC", Type: "string", JSONPath: ".status.network.plcDirectoryUrl"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketBidder", Plural: "marketbidders", Revision: 22, Export: "applications", Object: MarketBidder{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "DID", Type: "string", JSONPath: ".status.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "MarketRequester", Plural: "marketrequesters", Revision: 19, Export: "applications", Object: MarketRequester{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "DID", Type: "string", JSONPath: ".status.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ComputeContract", Plural: "computecontracts", Revision: 10, Export: "computecontracts", Object: ComputeContract{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Contract", Type: "string", JSONPath: ".status.contract.phase"},
		{Name: "Winner", Type: "string", JSONPath: ".status.contract.winner.did"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Gateway", Plural: "gateways", Revision: 24, Export: "applications", Object: Gateway{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "SshGateway", Plural: "sshgateways", Revision: 18, Export: "applications", Object: SshGateway{}, PrinterColumns: []PrinterColumn{
		{Name: "Profile", Type: "string", JSONPath: ".spec.profile"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Bidder", Plural: "bidders", Revision: 13, Export: "applications", Object: Bidder{}, PrinterColumns: componentPrinterColumns},
	{Kind: "GuestControlPlane", Plural: "guestcontrolplanes", Revision: 7, Export: "applications", Object: GuestControlPlane{}, PrinterColumns: componentPrinterColumns},
	{Kind: "Pds", Plural: "pds", Revision: 8, Export: "applications", Object: Pds{}, PrinterColumns: componentPrinterColumns},
	{Kind: "Requester", Plural: "requesters", Revision: 16, Export: "applications", Object: Requester{}, PrinterColumns: componentPrinterColumns},
	{Kind: "GatewayAPI", Plural: "gatewayapis", Revision: 10, Export: "applications", Object: GatewayAPI{}, PrinterColumns: componentPrinterColumns},
	{Kind: "SshServer", Plural: "sshservers", Revision: 8, Export: "applications", Object: SshServer{}, PrinterColumns: componentPrinterColumns},
	{Kind: "Plc", Plural: "plcs", Revision: 7, Export: "applications", Object: Plc{}, PrinterColumns: componentPrinterColumns},
	{Kind: "Relay", Plural: "relays", Revision: 7, Export: "applications", Object: Relay{}, PrinterColumns: componentPrinterColumns},
	{Kind: "IngressProxy", Plural: "ingressproxies", Revision: 8, Export: "applications", Object: IngressProxy{}, PrinterColumns: componentPrinterColumns},
	{Kind: "DispatcherTLS", Plural: "dispatchertlses", Revision: 7, Export: "applications", Object: DispatcherTLS{}, PrinterColumns: componentPrinterColumns},
	{Kind: "MarketPresence", Plural: "marketpresences", Revision: 2, Export: "applications", Object: MarketPresence{}, PrinterColumns: []PrinterColumn{
		{Name: "Offering", Type: "string", JSONPath: ".spec.offeringUri"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Heartbeat", Type: "string", JSONPath: ".status.lastHeartbeatAt"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "KcpInstallation", Plural: "kcpinstallations", Revision: 1, Export: "applications", Object: KcpInstallation{}, PrinterColumns: []PrinterColumn{
		{Name: "SelectedSlot", Type: "string", JSONPath: ".spec.selectedSlot"},
		{Name: "ActiveSlot", Type: "string", JSONPath: ".status.activeSlot"},
		{Name: "FrontBackend", Type: "string", JSONPath: ".status.frontBackend"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "HostInstall", Plural: "hostinstalls", Revision: 1, Export: "applications", Object: HostInstall{}, PrinterColumns: []PrinterColumn{
		{Name: "Account", Type: "string", JSONPath: ".spec.account"},
		{Name: "Version", Type: "string", JSONPath: ".status.installedVersion"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "HostRelease", Plural: "hostreleases", Revision: 1, Export: "applications", Object: HostRelease{}, PrinterColumns: []PrinterColumn{
		{Name: "Current", Type: "string", JSONPath: ".status.current"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ChildApplication", Plural: "childapplications", Revision: 2, Export: "applications", Object: ChildApplication{}, PrinterColumns: []PrinterColumn{
		{Name: "Network", Type: "string", JSONPath: ".spec.networkRef.name"},
		{Name: "App", Type: "string", JSONPath: ".spec.app"},
		{Name: "Workspace", Type: "string", JSONPath: ".status.workspace"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoRepo", Plural: "atprotorepos", Revision: 4, Export: "applications", Object: AtprotoRepo{}, PrinterColumns: []PrinterColumn{
		{Name: "Backend", Type: "string", JSONPath: ".spec.backendRef.kind"},
		{Name: "DID", Type: "string", JSONPath: ".status.did"},
		{Name: "Head", Type: "string", JSONPath: ".status.head"},
		{Name: "Writable", Type: "boolean", JSONPath: ".status.writable"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "LocalRepo", Plural: "localrepos", Revision: 2, Export: "applications", Object: LocalRepo{}, Contracts: []string{commonv1.ContractAtprotoRepos}, PrinterColumns: []PrinterColumn{
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "PDS", Type: "string", JSONPath: ".spec.pdsRef.name"},
		{Name: "Head", Type: "string", JSONPath: ".status.head"},
		{Name: "Seq", Type: "integer", JSONPath: ".status.seq"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "RemoteRepo", Plural: "remoterepos", Revision: 2, Export: "applications", Object: RemoteRepo{}, Contracts: []string{commonv1.ContractAtprotoRepos}, PrinterColumns: []PrinterColumn{
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "PDS", Type: "string", JSONPath: ".spec.pdsRef.name"},
		{Name: "Credentials", Type: "string", JSONPath: ".spec.credentialsRef.name"},
		{Name: "Head", Type: "string", JSONPath: ".status.head"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PdsAccount", Plural: "pdsaccounts", Revision: 2, Export: "applications", Object: PdsAccount{}, PrinterColumns: []PrinterColumn{
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "Handle", Type: "string", JSONPath: ".spec.handle"},
		{Name: "Active", Type: "boolean", JSONPath: ".status.active"},
		{Name: "Sessions", Type: "integer", JSONPath: ".status.sessionCount"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "AtprotoSession", Plural: "atprotosessions", Revision: 2, Export: "applications", Object: AtprotoSession{}, PrinterColumns: []PrinterColumn{
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "Active", Type: "boolean", JSONPath: ".status.active"},
		{Name: "Expires", Type: "string", JSONPath: ".status.expiresAt"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PlcIdentity", Plural: "plcidentities", Revision: 1, Export: "applications", Object: PlcIdentity{}, PrinterColumns: []PrinterColumn{
		{Name: "DID", Type: "string", JSONPath: ".spec.did"},
		{Name: "Operations", Type: "integer", JSONPath: ".status.opCount"},
		{Name: "Tombstoned", Type: "boolean", JSONPath: ".status.tombstoned"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PortLease", Plural: "portleases", Revision: 2, Export: "applications", Object: PortLease{}, PrinterColumns: []PrinterColumn{
		{Name: "Purpose", Type: "string", JSONPath: ".spec.purpose"},
		{Name: "Port", Type: "integer", JSONPath: ".spec.port"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Holder", Type: "string", JSONPath: ".status.holderRef"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "SubnetLease", Plural: "subnetleases", Revision: 2, Export: "applications", Object: SubnetLease{}, PrinterColumns: []PrinterColumn{
		{Name: "Purpose", Type: "string", JSONPath: ".spec.purpose"},
		{Name: "Prefix", Type: "string", JSONPath: ".spec.prefix"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Holder", Type: "string", JSONPath: ".status.holderRef"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ClusterRoll", Plural: "clusterrolls", Revision: 2, Export: "applications", Object: ClusterRoll{}, PrinterColumns: []PrinterColumn{
		{Name: "Cluster", Type: "string", JSONPath: ".spec.clusterRef.name"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Started", Type: "string", JSONPath: ".status.startedAt"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "RelayStatus", Plural: "relaystatuses", Revision: 2, Export: "applications", Object: RelayStatus{}, PrinterColumns: []PrinterColumn{
		{Name: "Relay", Type: "string", JSONPath: ".spec.relayRef.name"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Cursor", Type: "integer", JSONPath: ".status.cursor"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "VaultCredential", Plural: "vaultcredentials", Revision: 2, Export: "applications", Object: VaultCredential{}, PrinterColumns: []PrinterColumn{
		{Name: "Server", Type: "string", JSONPath: ".spec.vault.name"},
		{Name: "Path", Type: "string", JSONPath: ".spec.path"},
		{Name: "RotationDue", Type: "boolean", JSONPath: ".status.rotationDue"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "VaultCertificate", Plural: "vaultcertificates", Revision: 2, Export: "applications", Object: VaultCertificate{}, PrinterColumns: []PrinterColumn{
		{Name: "CommonName", Type: "string", JSONPath: ".spec.commonName"},
		{Name: "Serial", Type: "string", JSONPath: ".status.serialNumber"},
		{Name: "Expires", Type: "string", JSONPath: ".status.expiresAt"},
		{Name: "RotationDue", Type: "boolean", JSONPath: ".status.rotationDue"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "StateSync", Plural: "statesyncs", Revision: 1, Export: "applications", Object: StateSync{}, PrinterColumns: []PrinterColumn{
		{Name: "Adapter", Type: "string", JSONPath: ".spec.sink.adapter"},
		{Name: "Direction", Type: "string", JSONPath: ".spec.direction"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Observed", Type: "boolean", JSONPath: ".status.observed.exists"},
		{Name: "Key", Type: "string", JSONPath: ".spec.idempotencyKey"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}

var componentPrinterColumns = []PrinterColumn{
	{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
	{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
	{Name: "Endpoint", Type: "string", JSONPath: ".status.endpoint"},
	{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
}

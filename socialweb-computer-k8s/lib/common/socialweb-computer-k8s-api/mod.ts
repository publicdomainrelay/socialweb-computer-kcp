export const GROUP_NAME = "socialweb.computer";
export const VERSION = "v1alpha1";
export const ANNOTATION_TTL_AFTER_FINISHED = "socialweb.computer/ttl-after-finished";
export const FINALIZER_ATPROTO_IDENTITY = "atprotoidentity.socialweb.computer/revocation";
export const CONDITION_REFERENCES_RESOLVED = "ReferencesResolved";
export const FINALIZER_ATPROTO_RECORD = "atprotorecord.socialweb.computer/repo";
export const CONDITION_VERIFIED = "Verified";
export const CONDITION_TEARDOWN_BLOCKED = "TeardownBlocked";
export const CONDITION_TENANT_PATH_SHUT = "TenantPathShut";
export const CONDITION_REACHABLE = "Reachable";
export const CONDITION_REVOCATION_PENDING = "RevocationPending";
export const CONDITION_IDENTITY_VERIFICATION_FAILED = "IdentityVerificationFailed";
export const CONDITION_NODE_PROVISION_BACKOFF = "NodeProvisionBackoff";
export const FINALIZER_CLUSTER = "cluster.socialweb.computer/teardown";
export const CONDITION_SIGNED_IN = "SignedIn";
export const CONDITION_ASSOCIATED = "Associated";
export const CONDITION_NODE_JOINED = "NodeJoined";
export const FINALIZER_NODE = "node.socialweb.computer/teardown";
export const FINALIZER_NODE_CLAIM = "nodeclaim.socialweb.computer/release";

export const CLUSTER_PHASES = ["Initializing", "Ready", "Unavailable", "Draining"] as const;
export type ClusterPhase = typeof CLUSTER_PHASES[number];

export const NODE_PHASES = ["Pending", "Running", "Draining"] as const;
export type NodePhase = typeof NODE_PHASES[number];

export const NODE_CLAIM_PHASES = ["Pending", "Bidding", "Provisioning", "Joining", "Running", "Failed", "Released", "Lost", "Adopting"] as const;
export type NodeClaimPhase = typeof NODE_CLAIM_PHASES[number];

export const CONTRACT_PHASES = ["requested", "rfp_submitted", "winner_selected", "accepted", "receipt", "network", "released"] as const;
export type ContractPhase = typeof CONTRACT_PHASES[number];

export const CONTRACT_OUTCOMES = ["no_bids", "bids_denied", "policy_rejected", "returned", "receipt_invalid", "network_timeout", "complete"] as const;
export type ContractOutcome = typeof CONTRACT_OUTCOMES[number];

export const CLUSTER_LINK_SERVICES = ["apiserver", "openbao"] as const;
export type ClusterLinkService = typeof CLUSTER_LINK_SERVICES[number];

export const CLUSTER_LINK_PHASES = ["Pending", "Listening", "Failed"] as const;
export type ClusterLinkPhase = typeof CLUSTER_LINK_PHASES[number];

export const IDENTITY_SCOPES = ["Account", "Cluster"] as const;
export type IdentityScope = typeof IDENTITY_SCOPES[number];

export const IDENTITY_STATES = ["Active", "Revoked", "RevocationPending", "ColdDeferred"] as const;
export type IdentityState = typeof IDENTITY_STATES[number];

export const MARKET_IDENTITY_MODES = ["oauthQr", "oauthBrowser", "pdsAccount", "localRepo", "oauthDelegated"] as const;
export type MarketIdentityMode = typeof MARKET_IDENTITY_MODES[number];

export const ASSOCIATION_MODES = ["RequireApproval", "Skip"] as const;
export type AssociationMode = typeof ASSOCIATION_MODES[number];

export const MARKET_IDENTITY_PHASES = ["Pending", "AwaitingScan", "AwaitingAuthorization", "AwaitingApproval", "Ready", "SessionExpired", "Failed"] as const;
export type MarketIdentityPhase = typeof MARKET_IDENTITY_PHASES[number];

export const PENDING_ACTION_KINDS = ["scanQr", "visitUrl", "approveAssociation", "signInAgain"] as const;
export type PendingActionKind = typeof PENDING_ACTION_KINDS[number];

export const MISSING_REFERENCE_POLICYS = ["Keep", "Delete"] as const;
export type MissingReferencePolicy = typeof MISSING_REFERENCE_POLICYS[number];

export const ATPROTO_RECORD_PHASES = ["Pending", "Applied", "Failed"] as const;
export type AtprotoRecordPhase = typeof ATPROTO_RECORD_PHASES[number];

export const APPLICATION_KINDS = ["MarketBidder", "MarketRequester", "Gateway", "SshGateway"] as const;
export type ApplicationKind = typeof APPLICATION_KINDS[number];

export const APPLICATION_PHASES = ["Pending", "Provisioning", "Ready", "Deleting", "Failed"] as const;
export type ApplicationPhase = typeof APPLICATION_PHASES[number];

export const COMPUTE_CONTRACT_PHASES = ["Pending", "Requesting", "Held", "Releasing", "Released", "Failed"] as const;
export type ComputeContractPhase = typeof COMPUTE_CONTRACT_PHASES[number];

export const CONTRACT_BID_VERDICTS = ["considered", "denied", "chosen", "notChosen", "policyRejected"] as const;
export type ContractBidVerdict = typeof CONTRACT_BID_VERDICTS[number];

export const CONTRACT_CHOSEN_REASONS = ["firstFree", "bestWithinWindow", "policy", "resumed"] as const;
export type ContractChosenReason = typeof CONTRACT_CHOSEN_REASONS[number];

export interface ObjectMeta {
  name?: string;
  generateName?: string;
  namespace?: string;
  uid?: string;
  resourceVersion?: string;
  generation?: number;
  creationTimestamp?: string;
  deletionTimestamp?: string;
  deletionGracePeriodSeconds?: number;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  finalizers?: string[];
  [key: string]: unknown;
}

export type Condition = Record<string, unknown>;

export interface Cluster {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: ClusterSpec;
  status?: ClusterStatus;
}

export interface ClusterSpec {
  account: string;
  running?: boolean;
}

export interface ClusterStatus {
  phase?: ClusterPhase;
  URL?: string;
  instance?: string;
  nodeExpiry?: number;
  conditions?: Condition[];
}

export interface Node {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: NodeSpec;
  status?: NodeStatus;
}

export interface NodeSpec {
  cluster: string;
}

export interface NodeStatus {
  phase?: NodePhase;
  nodeExpiry?: number;
  conditions?: Condition[];
}

export interface NodeClaim {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: NodeClaimSpec;
  status?: NodeClaimStatus;
}

export interface NodeClaimSpec {
  cluster: string;
  nodeName: string;
  ownerDid: string;
  apiserverPort: number;
  advertiseAddress: string;
  tokenFile: string;
  nodePasswordFile: string;
  serverCAFile: string;
  denyBidderDids?: string[];
}

export interface NodeClaimStatus {
  phase?: NodeClaimPhase;
  contract?: NodeClaimContract;
  fqdn?: string;
  message?: string;
  grant?: NodeClaimGrant;
  secrets?: NodeClaimSecrets;
}

export interface NodeClaimContract {
  vmUri?: string;
  rfpUri?: string;
  acceptUri?: string;
  receiptUri?: string;
  receiptCid?: string;
  submitEventRef?: string;
  version?: number;
  phase?: ContractPhase;
  vmName?: string;
  requesterDid?: string;
  marketDid?: string;
  payload?: RecordRef;
  rfp?: RecordRef;
  policy?: RecordRef;
  bid?: RecordRef;
  winner?: ContractWinner;
  acceptPending?: ContractAcceptPending;
  accept?: RecordRef;
  receipt?: RecordRef;
  receiptOk?: boolean;
  vmAddress?: string;
  bids?: number;
  release?: ContractRelease;
  termination?: ContractTermination;
  hostKeys?: ContractHostKeys;
  outcome?: ContractOutcome;
  error?: string;
  timestamps?: ContractTimestamps;
}

export interface RecordRef {
  uri: string;
  cid: string;
}

export interface ContractWinner {
  did: string;
  submitAccept?: string;
}

export interface ContractAcceptPending {
  bidUri: string;
}

export interface ContractRelease {
  event: RecordRef;
  payload: RecordRef;
  ok: boolean;
  status?: number;
}

export interface ContractTermination {
  reason: string;
  exitCode?: number;
  observedAt: string;
  event: RecordRef;
  payload: RecordRef;
  request?: RecordRef;
}

export interface ContractHostKeys {
  keys: ContractHostKey[];
  observedAt: string;
  event: RecordRef;
  payload: RecordRef;
}

export interface ContractHostKey {
  type: string;
  publicKey: string;
  fingerprint: string;
}

export interface ContractTimestamps {
  requested?: string;
  rfp_submitted?: string;
  winner_selected?: string;
  accepted?: string;
  receipt?: string;
  network?: string;
  released?: string;
}

export interface NodeClaimGrant {
  issuer: string;
  subject: string;
  audience: string;
  role?: string;
}

export interface NodeClaimSecrets {
  ingressDid: string;
  keyRef: string;
  url: string;
  servedAt?: string;
}

export interface ClusterLink {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: ClusterLinkSpec;
  status?: ClusterLinkStatus;
}

export interface ClusterLinkSpec {
  cluster: string;
  service: ClusterLinkService;
  targetPort: number;
}

export interface ClusterLinkStatus {
  phase?: ClusterLinkPhase;
  endpointId?: string;
  did?: string;
  ticket?: string;
  message?: string;
}

export interface ClusterClientCertificate {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: ClusterClientCertificateSpec;
  status?: ClusterClientCertificateStatus;
}

export interface ClusterClientCertificateSpec {
  cluster: string;
  did: string;
  serial: string;
  notAfter: string;
  noExpiry?: boolean;
  revoked?: boolean;
}

export interface ClusterClientCertificateStatus {
  revokedAt?: string;
  lastError?: string;
}

export interface AtprotoIdentity {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: AtprotoIdentitySpec;
  status?: AtprotoIdentityStatus;
}

export interface AtprotoIdentitySpec {
  did: string;
  scope: IdentityScope;
  cluster?: string;
  revoked?: boolean;
}

export interface AtprotoIdentityStatus {
  state?: IdentityState;
  conditions?: Condition[];
}

export interface MarketIdentity {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: MarketIdentitySpec;
  status?: MarketIdentityStatus;
}

export interface MarketIdentitySpec {
  mode: MarketIdentityMode;
  handle?: string;
  pdsUrl?: string;
  association?: AssociationMode;
  associateWith?: string;
  approvedAssociations?: string[];
  rejectedAssociations?: string[];
  associationWatch?: string;
  keysRef?: VaultRef;
  sessionRef?: VaultRef;
}

export interface VaultRef {
  mount: string;
  path: string;
}

export interface MarketIdentityStatus {
  phase?: MarketIdentityPhase;
  observedGeneration?: number;
  did?: string;
  handle?: string;
  pdsUrl?: string;
  serviceDid?: string;
  pendingAction?: MarketIdentityPendingAction;
  associations?: string[];
  sessionRef?: VaultRef;
  message?: string;
  conditions?: Condition[];
}

export interface MarketIdentityPendingAction {
  kind: PendingActionKind;
  url?: string;
  callerDid?: string;
  handle?: string;
  expiresAt?: string;
}

export interface AtprotoRecord {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: AtprotoRecordSpec;
  status?: AtprotoRecordStatus;
}

export interface AtprotoRecordSpec {
  repo: string;
  collection: string;
  rkey: string;
  record: Record<string, unknown>;
  onMissingReference?: MissingReferencePolicy;
}

export interface AtprotoRecordStatus {
  phase?: AtprotoRecordPhase;
  observedGeneration?: number;
  did?: string;
  uri?: string;
  cid?: string;
  message?: string;
  conditions?: Condition[];
}

export interface AtprotoRecordWatch {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: AtprotoRecordWatchSpec;
  status?: AtprotoRecordWatchStatus;
}

export interface AtprotoRecordWatchSpec {
  repo: string;
  collection: string;
  match?: AtprotoRecordMatch[];
  source: AtprotoRecordWatchSource;
}

export interface AtprotoRecordMatch {
  fieldPath: string;
  value?: string;
  valueFrom?: AtprotoValueFrom;
}

export interface AtprotoValueFrom {
  kind?: string;
  resource?: string;
  apiVersion?: string;
  name: string;
  fieldPath: string;
  workspace?: string;
  namespace?: string;
}

export interface AtprotoRecordWatchSource {
  relayUrls?: string[];
  jetstreamUrls?: string[];
}

export interface AtprotoRecordWatchStatus {
  observedGeneration?: number;
  records?: WatchedRecord[];
  cursors?: AtprotoRecordWatchCursor[];
  message?: string;
  conditions?: Condition[];
}

export interface WatchedRecord {
  did: string;
  handle?: string;
  uri: string;
  cid?: string;
  value: Record<string, unknown>;
  verifiedAt: string;
}

export interface AtprotoRecordWatchCursor {
  source: string;
  cursor: number;
}

export interface MarketNetwork {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: MarketNetworkSpec;
  status?: MarketNetworkStatus;
}

export interface MarketNetworkSpec {
  profile: string;
  guestLabel?: string;
  apps?: NetworkApp[];
}

export interface NetworkApp {
  name: string;
  kind: ApplicationKind;
  profile: string;
  policy?: string;
  firecracker?: FirecrackerProvider;
  kcp?: KcpProvider;
  options?: ApplicationOption[];
  quota?: ApplicationQuota;
  ownerSessionsFile?: string;
  clusterProvider?: GatewayClusterProvider;
  stopped?: boolean;
}

export interface FirecrackerProvider {
  workRoot: string;
  vmruntimeCommand: string[];
}

export interface KcpProvider {
  handle?: string;
  vmHostBackend?: string;
  guestImageSource: string;
  stopGraceSeconds?: number;
}

export interface ApplicationOption {
  name: string;
  value?: string;
}

export interface ApplicationQuota {
  bids?: number;
  computeVMs?: number;
  computeContracts?: number;
  nodeClaims?: number;
  policyWorkflowRuns?: number;
  denoRuns?: number;
}

export interface GatewayClusterProvider {
  scaleHold?: string;
  scaleMaxNodes?: number;
  scalePodsPerNode?: number;
}

export interface MarketNetworkStatus {
  observedGeneration?: number;
  phase?: ApplicationPhase;
  message?: string;
  network?: MarketNetworkValue;
  bidderPdsUrl?: string;
  components?: ApplicationComponent[];
  listeners?: ApplicationListener[];
  apps?: NetworkAppStatus[];
  stateDir?: StateDirUsage;
  gc?: GCStatus;
  ttl?: TTLStatus;
  conditions?: Condition[];
}

export interface MarketNetworkValue {
  plcDirectoryUrl: string;
  ingressProxyHost: string;
  relayUrls: string[];
  firehoseUrls: string[];
  qrOrigin: string;
  guestTlsUrl?: string;
  caBundleRef?: string;
  hosts?: NetworkHost[];
}

export interface NetworkHost {
  name: string;
  address: string;
}

export interface ApplicationComponent {
  kind: string;
  name: string;
  workspace: string;
  phase?: string;
  ready: boolean;
}

export interface ApplicationListener {
  component: string;
  name: string;
  kind: string;
  address: string;
}

export interface NetworkAppStatus {
  name: string;
  kind: ApplicationKind;
  instance?: string;
  kcpUrl?: string;
  kcpRoot?: string;
  phase?: ApplicationPhase;
  message?: string;
  did?: string;
  components?: ApplicationComponent[];
  listeners?: ApplicationListener[];
}

export interface StateDirUsage {
  path: string;
  bytes: number;
  files: number;
  sampledAt: string;
}

export interface GCStatus {
  sampledAt: string;
  objects: number;
  orphanTotal: number;
  orphans?: GCKindCount[];
  stuckTotal: number;
  terminating?: GCStuck[];
}

export interface GCKindCount {
  kind: string;
  count: number;
  examples?: string[];
}

export interface GCStuck {
  kind: string;
  finalizer: string;
  controller: string;
  count: number;
  examples?: string[];
}

export interface TTLStatus {
  sampledAt: string;
  tracked: number;
  deletedTotal: number;
  deleted?: TTLDeleted[];
}

export interface TTLDeleted {
  kind: string;
  count: number;
  lastAt?: string;
}

export interface MarketBidder {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: MarketBidderSpec;
  status?: ApplicationStatus;
}

export interface MarketBidderSpec {
  profile: string;
  network?: MarketNetworkValue;
  identity: ApplicationIdentity;
  policy?: string;
  firecracker?: FirecrackerProvider;
  kcp?: KcpProvider;
  signingKeyRotation?: number;
  options?: ApplicationOption[];
  quota?: ApplicationQuota;
  operator?: string;
}

export interface ApplicationIdentity {
  mode: MarketIdentityMode;
  handle?: string;
  pdsUrl?: string;
  association?: AssociationMode;
}

export interface ApplicationStatus {
  observedGeneration?: number;
  phase?: ApplicationPhase;
  message?: string;
  profile?: string;
  did?: string;
  components?: ApplicationComponent[];
  listeners?: ApplicationListener[];
  stateDir?: StateDirUsage;
  store?: StoreUsage;
  gc?: GCStatus;
  ttl?: TTLStatus;
  quotas?: QuotaUsage[];
  conditions?: Condition[];
  market?: MarketLoopStatus;
}

export interface StoreUsage {
  kind: string;
  path?: string;
  bytes: number;
  freeBytes?: number;
  rows?: number;
  deadTuples?: number;
  autoVacuum?: string;
  vacuums?: number;
  reclaimedBytes?: number;
  lastVacuumAt?: string;
  sampledAt: string;
}

export interface QuotaUsage {
  workspace: string;
  resource: string;
  hard: number;
  used: number;
}

export interface MarketLoopStatus {
  rfpsSeen?: number;
  rfpsScoped?: number;
  declinedByReason?: MarketDeclineCount[];
  bids?: number;
  wins?: number;
  contractsActive?: number;
  streams?: MarketStreamStatus[];
  recentDeclines?: MarketDecline[];
  rfpToBid?: MarketLatencyStatus;
  kcpWait?: MarketLatencyStatus;
  pdsWrites?: PdsWriteStatus;
  updatedAt?: string;
}

export interface MarketDeclineCount {
  reason: string;
  count: number;
}

export interface MarketStreamStatus {
  source: string;
  cursor?: number;
  lagMs?: number;
  lastEventAt?: string;
}

export interface MarketDecline {
  rfpUri: string;
  reason: string;
  at: string;
}

export interface MarketLatencyStatus {
  count: number;
  p50Us: number;
  p95Us: number;
  maxUs: number;
}

export interface PdsWriteStatus {
  since: string;
  writes: number;
  points: number;
  rateLimited?: number;
  reserve: number;
  byCollection?: PdsWriteCollection[];
  perHour?: PdsWriteHour[];
  limits?: PdsLimitStatus[];
}

export interface PdsWriteCollection {
  collection: string;
  writes: number;
  points: number;
}

export interface PdsWriteHour {
  hour: string;
  points: number;
}

export interface PdsLimitStatus {
  pds: string;
  limit?: number;
  remaining?: number;
  resetAt?: string;
  pausedUntil?: string;
  observedAt: string;
}

export interface MarketRequester {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: MarketRequesterSpec;
  status?: ApplicationStatus;
}

export interface MarketRequesterSpec {
  profile: string;
  network?: MarketNetworkValue;
  identity: ApplicationIdentity;
  bidderDids?: string[];
  options?: ApplicationOption[];
  quota?: ApplicationQuota;
}

export interface ComputeContract {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: ComputeContractSpec;
  status?: ComputeContractStatus;
}

export interface ComputeContractSpec {
  vmName?: string;
  bidderDids?: string[];
  denyBidderDids?: string[];
  sshAuthorizedKeys?: string[];
  userData?: string;
  policy?: string;
  policyArgs?: string;
  vmReadyTimeoutSec?: number;
  secretsRef?: VaultRef;
  release?: boolean;
  ownerDid?: string;
  adopt?: boolean;
  nodeJoin?: ComputeContractNodeJoin;
}

export interface ComputeContractNodeJoin {
  cluster: string;
  meshd?: ComputeContractMeshd;
}

export interface ComputeContractMeshd {
  downloadBase: string;
  pins: ComputeContractMeshdPin[];
}

export interface ComputeContractMeshdPin {
  arch: string;
  sha256: string;
}

export interface ComputeContractStatus {
  observedGeneration?: number;
  phase?: ComputeContractPhase;
  message?: string;
  secretsUrl?: string;
  secretsFetchedAt?: string;
  contract?: NodeClaimContract;
  grant?: NodeClaimGrant;
  bids?: ContractBid[];
  chosen?: ContractChosen;
  conditions?: Condition[];
  execution?: ExecutionStatus;
}

export interface ContractBid {
  did: string;
  uri: string;
  offer?: string;
  cost?: number;
  verdict: ContractBidVerdict;
  arrivedAt: string;
}

export interface ContractChosen {
  uri: string;
  did: string;
  reason: ContractChosenReason;
  considered: number;
}

export interface ExecutionStatus {
  step?: string;
  stepStartedAt?: string;
  lastEventAt?: string;
  lastError?: string;
  attempts?: number;
}

export interface Gateway {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: GatewaySpec;
  status?: ApplicationStatus;
}

export interface GatewaySpec {
  profile: string;
  network?: MarketNetworkValue;
  bidderDids?: string[];
  options?: ApplicationOption[];
  quota?: ApplicationQuota;
  marketNetwork?: GatewayMarketNetwork;
  webAppFront?: string;
  clusterProvider?: GatewayClusterProvider;
  operator?: string;
}

export interface GatewayMarketNetwork {
  ownerSessionsFile?: string;
  nodeGuestHostAliases?: string[];
  nodeUserDataBaseFile?: string;
}

export interface SshGateway {
  apiVersion?: string;
  kind?: string;
  metadata?: ObjectMeta;
  spec?: SshGatewaySpec;
  status?: ApplicationStatus;
}

export interface SshGatewaySpec {
  profile: string;
  network?: MarketNetworkValue;
  bidderDids?: string[];
  options?: ApplicationOption[];
  quota?: ApplicationQuota;
  ownerSessionsFile?: string;
}

export interface ApiResource {
  group: string;
  version: string;
  kind: string;
  listKind: string;
  plural: string;
  singular: string;
  schema: string;
  export: string;
}

export const CLUSTER_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "Cluster",
  listKind: "ClusterList",
  plural: "clusters",
  singular: "cluster",
  schema: "v1alpha1-1.clusters.socialweb.computer",
  export: "clusters",
};

export const NODE_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "Node",
  listKind: "NodeList",
  plural: "nodes",
  singular: "node",
  schema: "v1alpha1-1.nodes.socialweb.computer",
  export: "clusters",
};

export const NODE_CLAIM_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "NodeClaim",
  listKind: "NodeClaimList",
  plural: "nodeclaims",
  singular: "nodeclaim",
  schema: "v1alpha1-5.nodeclaims.socialweb.computer",
  export: "clusters",
};

export const CLUSTER_LINK_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "ClusterLink",
  listKind: "ClusterLinkList",
  plural: "clusterlinks",
  singular: "clusterlink",
  schema: "v1alpha1-1.clusterlinks.socialweb.computer",
  export: "clusters",
};

export const CLUSTER_CLIENT_CERTIFICATE_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "ClusterClientCertificate",
  listKind: "ClusterClientCertificateList",
  plural: "clusterclientcertificates",
  singular: "clusterclientcertificate",
  schema: "v1alpha1-1.clusterclientcertificates.socialweb.computer",
  export: "clusters",
};

export const ATPROTO_IDENTITY_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "AtprotoIdentity",
  listKind: "AtprotoIdentityList",
  plural: "atprotoidentities",
  singular: "atprotoidentity",
  schema: "v1alpha1-1.atprotoidentities.socialweb.computer",
  export: "atprotoidentities",
};

export const MARKET_IDENTITY_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "MarketIdentity",
  listKind: "MarketIdentityList",
  plural: "marketidentities",
  singular: "marketidentity",
  schema: "v1alpha1-2.marketidentities.socialweb.computer",
  export: "marketidentities",
};

export const ATPROTO_RECORD_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "AtprotoRecord",
  listKind: "AtprotoRecordList",
  plural: "atprotorecords",
  singular: "atprotorecord",
  schema: "v1alpha1-2.atprotorecords.socialweb.computer",
  export: "marketidentities",
};

export const ATPROTO_RECORD_WATCH_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "AtprotoRecordWatch",
  listKind: "AtprotoRecordWatchList",
  plural: "atprotorecordwatches",
  singular: "atprotorecordwatch",
  schema: "v1alpha1-1.atprotorecordwatches.socialweb.computer",
  export: "marketidentities",
};

export const MARKET_NETWORK_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "MarketNetwork",
  listKind: "MarketNetworkList",
  plural: "marketnetworks",
  singular: "marketnetwork",
  schema: "v1alpha1-9.marketnetworks.socialweb.computer",
  export: "applications",
};

export const MARKET_BIDDER_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "MarketBidder",
  listKind: "MarketBidderList",
  plural: "marketbidders",
  singular: "marketbidder",
  schema: "v1alpha1-12.marketbidders.socialweb.computer",
  export: "applications",
};

export const MARKET_REQUESTER_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "MarketRequester",
  listKind: "MarketRequesterList",
  plural: "marketrequesters",
  singular: "marketrequester",
  schema: "v1alpha1-9.marketrequesters.socialweb.computer",
  export: "applications",
};

export const COMPUTE_CONTRACT_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "ComputeContract",
  listKind: "ComputeContractList",
  plural: "computecontracts",
  singular: "computecontract",
  schema: "v1alpha1-4.computecontracts.socialweb.computer",
  export: "computecontracts",
};

export const GATEWAY_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "Gateway",
  listKind: "GatewayList",
  plural: "gateways",
  singular: "gateway",
  schema: "v1alpha1-13.gateways.socialweb.computer",
  export: "applications",
};

export const SSH_GATEWAY_RESOURCE: ApiResource = {
  group: "socialweb.computer",
  version: "v1alpha1",
  kind: "SshGateway",
  listKind: "SshGatewayList",
  plural: "sshgateways",
  singular: "sshgateway",
  schema: "v1alpha1-7.sshgateways.socialweb.computer",
  export: "applications",
};

export const API_RESOURCES: readonly ApiResource[] = [CLUSTER_RESOURCE, NODE_RESOURCE, NODE_CLAIM_RESOURCE, CLUSTER_LINK_RESOURCE, CLUSTER_CLIENT_CERTIFICATE_RESOURCE, ATPROTO_IDENTITY_RESOURCE, MARKET_IDENTITY_RESOURCE, ATPROTO_RECORD_RESOURCE, ATPROTO_RECORD_WATCH_RESOURCE, MARKET_NETWORK_RESOURCE, MARKET_BIDDER_RESOURCE, MARKET_REQUESTER_RESOURCE, COMPUTE_CONTRACT_RESOURCE, GATEWAY_RESOURCE, SSH_GATEWAY_RESOURCE];

export type ApiTypeName =
  | "ApplicationComponent"
  | "ApplicationIdentity"
  | "ApplicationKind"
  | "ApplicationListener"
  | "ApplicationOption"
  | "ApplicationPhase"
  | "ApplicationQuota"
  | "ApplicationStatus"
  | "AssociationMode"
  | "AtprotoIdentity"
  | "AtprotoIdentitySpec"
  | "AtprotoIdentityStatus"
  | "AtprotoRecord"
  | "AtprotoRecordMatch"
  | "AtprotoRecordPhase"
  | "AtprotoRecordSpec"
  | "AtprotoRecordStatus"
  | "AtprotoRecordWatch"
  | "AtprotoRecordWatchCursor"
  | "AtprotoRecordWatchSource"
  | "AtprotoRecordWatchSpec"
  | "AtprotoRecordWatchStatus"
  | "AtprotoValueFrom"
  | "Cluster"
  | "ClusterClientCertificate"
  | "ClusterClientCertificateSpec"
  | "ClusterClientCertificateStatus"
  | "ClusterLink"
  | "ClusterLinkPhase"
  | "ClusterLinkService"
  | "ClusterLinkSpec"
  | "ClusterLinkStatus"
  | "ClusterPhase"
  | "ClusterSpec"
  | "ClusterStatus"
  | "ComputeContract"
  | "ComputeContractMeshd"
  | "ComputeContractMeshdPin"
  | "ComputeContractNodeJoin"
  | "ComputeContractPhase"
  | "ComputeContractSpec"
  | "ComputeContractStatus"
  | "Condition"
  | "ContractAcceptPending"
  | "ContractBid"
  | "ContractBidVerdict"
  | "ContractChosen"
  | "ContractChosenReason"
  | "ContractHostKey"
  | "ContractHostKeys"
  | "ContractOutcome"
  | "ContractPhase"
  | "ContractRelease"
  | "ContractTermination"
  | "ContractTimestamps"
  | "ContractWinner"
  | "ExecutionStatus"
  | "FirecrackerProvider"
  | "GCKindCount"
  | "GCStatus"
  | "GCStuck"
  | "Gateway"
  | "GatewayClusterProvider"
  | "GatewayMarketNetwork"
  | "GatewaySpec"
  | "IdentityScope"
  | "IdentityState"
  | "KcpProvider"
  | "MarketBidder"
  | "MarketBidderSpec"
  | "MarketDecline"
  | "MarketDeclineCount"
  | "MarketIdentity"
  | "MarketIdentityMode"
  | "MarketIdentityPendingAction"
  | "MarketIdentityPhase"
  | "MarketIdentitySpec"
  | "MarketIdentityStatus"
  | "MarketLatencyStatus"
  | "MarketLoopStatus"
  | "MarketNetwork"
  | "MarketNetworkSpec"
  | "MarketNetworkStatus"
  | "MarketNetworkValue"
  | "MarketRequester"
  | "MarketRequesterSpec"
  | "MarketStreamStatus"
  | "MissingReferencePolicy"
  | "NetworkApp"
  | "NetworkAppStatus"
  | "NetworkHost"
  | "Node"
  | "NodeClaim"
  | "NodeClaimContract"
  | "NodeClaimGrant"
  | "NodeClaimPhase"
  | "NodeClaimSecrets"
  | "NodeClaimSpec"
  | "NodeClaimStatus"
  | "NodePhase"
  | "NodeSpec"
  | "NodeStatus"
  | "ObjectMeta"
  | "PdsLimitStatus"
  | "PdsWriteCollection"
  | "PdsWriteHour"
  | "PdsWriteStatus"
  | "PendingActionKind"
  | "QuotaUsage"
  | "RecordRef"
  | "SshGateway"
  | "SshGatewaySpec"
  | "StateDirUsage"
  | "StoreUsage"
  | "TTLDeleted"
  | "TTLStatus"
  | "VaultRef"
  | "WatchedRecord";

type Shape =
  | { type: "string"; enum?: readonly string[] }
  | { type: "integer" }
  | { type: "boolean" }
  | { type: "array"; items: Shape }
  | { type: "object"; open: true }
  | { type: "object"; properties: Readonly<Record<string, Shape>>; required: readonly string[] }
  | { ref: ApiTypeName };

export interface ApiIssue {
  path: string;
  message: string;
}

export class ApiValidationError extends Error {
  constructor(readonly typeName: ApiTypeName, readonly issues: readonly ApiIssue[]) {
    super(`invalid ${typeName}: ${issues.map((i) => `${i.path || "(root)"}: ${i.message}`).join("; ")}`);
    this.name = "ApiValidationError";
  }
}

function joinPath(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}

function describe(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  switch (typeof value) {
    case "object":
      return "an object";
    case "string":
      return "a string";
    case "boolean":
      return "a boolean";
    case "number":
      return "a number";
    default:
      return typeof value;
  }
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function check(shape: Shape, value: unknown, path: string, out: ApiIssue[]): void {
  if ("ref" in shape) {
    check(SHAPES[shape.ref], value, path, out);
    return;
  }
  switch (shape.type) {
    case "object": {
      if (!isPlainObject(value)) {
        out.push({ path, message: `expected an object, got ${describe(value)}` });
        return;
      }
      if ("open" in shape) return;
      for (const [name, prop] of Object.entries(shape.properties)) {
        const v = value[name];
        if (v === undefined) {
          if (shape.required.includes(name)) out.push({ path: joinPath(path, name), message: "missing required field" });
          continue;
        }
        check(prop, v, joinPath(path, name), out);
      }
      for (const key of Object.keys(value).sort()) {
        if (!(key in shape.properties) && value[key] !== undefined) {
          out.push({ path: joinPath(path, key), message: "unknown field" });
        }
      }
      return;
    }
    case "array": {
      if (!Array.isArray(value)) {
        out.push({ path, message: `expected an array, got ${describe(value)}` });
        return;
      }
      value.forEach((item, i) => check(shape.items, item, `${path}[${i}]`, out));
      return;
    }
    case "string": {
      if (typeof value !== "string") {
        out.push({ path, message: `expected a string, got ${describe(value)}` });
        return;
      }
      if (shape.enum && !shape.enum.includes(value)) {
        out.push({ path, message: `${JSON.stringify(value)} is not one of ${shape.enum.join(", ")}` });
      }
      return;
    }
    case "integer": {
      if (typeof value !== "number" || !Number.isInteger(value)) {
        out.push({ path, message: `expected an integer, got ${describe(value)}` });
      }
      return;
    }
    case "boolean": {
      if (typeof value !== "boolean") {
        out.push({ path, message: `expected a boolean, got ${describe(value)}` });
      }
      return;
    }
  }
}

export function validate(typeName: ApiTypeName, value: unknown): ApiIssue[] {
  const out: ApiIssue[] = [];
  check(SHAPES[typeName], value, "", out);
  return out;
}

const SHAPES: Readonly<Record<ApiTypeName, Shape>> = {
  "ApplicationComponent": {
    "properties": {
      "kind": {
        "type": "string"
      },
      "name": {
        "type": "string"
      },
      "phase": {
        "type": "string"
      },
      "ready": {
        "type": "boolean"
      },
      "workspace": {
        "type": "string"
      }
    },
    "required": [
      "kind",
      "name",
      "workspace",
      "ready"
    ],
    "type": "object"
  },
  "ApplicationIdentity": {
    "properties": {
      "association": {
        "ref": "AssociationMode"
      },
      "handle": {
        "type": "string"
      },
      "mode": {
        "ref": "MarketIdentityMode"
      },
      "pdsUrl": {
        "type": "string"
      }
    },
    "required": [
      "mode"
    ],
    "type": "object"
  },
  "ApplicationKind": {
    "enum": [
      "MarketBidder",
      "MarketRequester",
      "Gateway",
      "SshGateway"
    ],
    "type": "string"
  },
  "ApplicationListener": {
    "properties": {
      "address": {
        "type": "string"
      },
      "component": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "name": {
        "type": "string"
      }
    },
    "required": [
      "component",
      "name",
      "kind",
      "address"
    ],
    "type": "object"
  },
  "ApplicationOption": {
    "properties": {
      "name": {
        "type": "string"
      },
      "value": {
        "type": "string"
      }
    },
    "required": [
      "name"
    ],
    "type": "object"
  },
  "ApplicationPhase": {
    "enum": [
      "Pending",
      "Provisioning",
      "Ready",
      "Deleting",
      "Failed"
    ],
    "type": "string"
  },
  "ApplicationQuota": {
    "properties": {
      "bids": {
        "type": "integer"
      },
      "computeContracts": {
        "type": "integer"
      },
      "computeVMs": {
        "type": "integer"
      },
      "denoRuns": {
        "type": "integer"
      },
      "nodeClaims": {
        "type": "integer"
      },
      "policyWorkflowRuns": {
        "type": "integer"
      }
    },
    "required": [],
    "type": "object"
  },
  "ApplicationStatus": {
    "properties": {
      "components": {
        "items": {
          "ref": "ApplicationComponent"
        },
        "type": "array"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "did": {
        "type": "string"
      },
      "gc": {
        "ref": "GCStatus"
      },
      "listeners": {
        "items": {
          "ref": "ApplicationListener"
        },
        "type": "array"
      },
      "market": {
        "ref": "MarketLoopStatus"
      },
      "message": {
        "type": "string"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "phase": {
        "ref": "ApplicationPhase"
      },
      "profile": {
        "type": "string"
      },
      "quotas": {
        "items": {
          "ref": "QuotaUsage"
        },
        "type": "array"
      },
      "stateDir": {
        "ref": "StateDirUsage"
      },
      "store": {
        "ref": "StoreUsage"
      },
      "ttl": {
        "ref": "TTLStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "AssociationMode": {
    "enum": [
      "RequireApproval",
      "Skip"
    ],
    "type": "string"
  },
  "AtprotoIdentity": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "AtprotoIdentitySpec"
      },
      "status": {
        "ref": "AtprotoIdentityStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoIdentitySpec": {
    "properties": {
      "cluster": {
        "type": "string"
      },
      "did": {
        "type": "string"
      },
      "revoked": {
        "type": "boolean"
      },
      "scope": {
        "ref": "IdentityScope"
      }
    },
    "required": [
      "did",
      "scope"
    ],
    "type": "object"
  },
  "AtprotoIdentityStatus": {
    "properties": {
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "state": {
        "ref": "IdentityState"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoRecord": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "AtprotoRecordSpec"
      },
      "status": {
        "ref": "AtprotoRecordStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoRecordMatch": {
    "properties": {
      "fieldPath": {
        "type": "string"
      },
      "value": {
        "type": "string"
      },
      "valueFrom": {
        "ref": "AtprotoValueFrom"
      }
    },
    "required": [
      "fieldPath"
    ],
    "type": "object"
  },
  "AtprotoRecordPhase": {
    "enum": [
      "Pending",
      "Applied",
      "Failed"
    ],
    "type": "string"
  },
  "AtprotoRecordSpec": {
    "properties": {
      "collection": {
        "type": "string"
      },
      "onMissingReference": {
        "ref": "MissingReferencePolicy"
      },
      "record": {
        "open": true,
        "type": "object"
      },
      "repo": {
        "type": "string"
      },
      "rkey": {
        "type": "string"
      }
    },
    "required": [
      "repo",
      "collection",
      "rkey",
      "record"
    ],
    "type": "object"
  },
  "AtprotoRecordStatus": {
    "properties": {
      "cid": {
        "type": "string"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "did": {
        "type": "string"
      },
      "message": {
        "type": "string"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "phase": {
        "ref": "AtprotoRecordPhase"
      },
      "uri": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoRecordWatch": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "AtprotoRecordWatchSpec"
      },
      "status": {
        "ref": "AtprotoRecordWatchStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoRecordWatchCursor": {
    "properties": {
      "cursor": {
        "type": "integer"
      },
      "source": {
        "type": "string"
      }
    },
    "required": [
      "source",
      "cursor"
    ],
    "type": "object"
  },
  "AtprotoRecordWatchSource": {
    "properties": {
      "jetstreamUrls": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "relayUrls": {
        "items": {
          "type": "string"
        },
        "type": "array"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoRecordWatchSpec": {
    "properties": {
      "collection": {
        "type": "string"
      },
      "match": {
        "items": {
          "ref": "AtprotoRecordMatch"
        },
        "type": "array"
      },
      "repo": {
        "type": "string"
      },
      "source": {
        "ref": "AtprotoRecordWatchSource"
      }
    },
    "required": [
      "repo",
      "collection",
      "source"
    ],
    "type": "object"
  },
  "AtprotoRecordWatchStatus": {
    "properties": {
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "cursors": {
        "items": {
          "ref": "AtprotoRecordWatchCursor"
        },
        "type": "array"
      },
      "message": {
        "type": "string"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "records": {
        "items": {
          "ref": "WatchedRecord"
        },
        "type": "array"
      }
    },
    "required": [],
    "type": "object"
  },
  "AtprotoValueFrom": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "fieldPath": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "name": {
        "type": "string"
      },
      "namespace": {
        "type": "string"
      },
      "resource": {
        "type": "string"
      },
      "workspace": {
        "type": "string"
      }
    },
    "required": [
      "name",
      "fieldPath"
    ],
    "type": "object"
  },
  "Cluster": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "ClusterSpec"
      },
      "status": {
        "ref": "ClusterStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "ClusterClientCertificate": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "ClusterClientCertificateSpec"
      },
      "status": {
        "ref": "ClusterClientCertificateStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "ClusterClientCertificateSpec": {
    "properties": {
      "cluster": {
        "type": "string"
      },
      "did": {
        "type": "string"
      },
      "noExpiry": {
        "type": "boolean"
      },
      "notAfter": {
        "type": "string"
      },
      "revoked": {
        "type": "boolean"
      },
      "serial": {
        "type": "string"
      }
    },
    "required": [
      "cluster",
      "did",
      "serial",
      "notAfter"
    ],
    "type": "object"
  },
  "ClusterClientCertificateStatus": {
    "properties": {
      "lastError": {
        "type": "string"
      },
      "revokedAt": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "ClusterLink": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "ClusterLinkSpec"
      },
      "status": {
        "ref": "ClusterLinkStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "ClusterLinkPhase": {
    "enum": [
      "Pending",
      "Listening",
      "Failed"
    ],
    "type": "string"
  },
  "ClusterLinkService": {
    "enum": [
      "apiserver",
      "openbao"
    ],
    "type": "string"
  },
  "ClusterLinkSpec": {
    "properties": {
      "cluster": {
        "type": "string"
      },
      "service": {
        "ref": "ClusterLinkService"
      },
      "targetPort": {
        "type": "integer"
      }
    },
    "required": [
      "cluster",
      "service",
      "targetPort"
    ],
    "type": "object"
  },
  "ClusterLinkStatus": {
    "properties": {
      "did": {
        "type": "string"
      },
      "endpointId": {
        "type": "string"
      },
      "message": {
        "type": "string"
      },
      "phase": {
        "ref": "ClusterLinkPhase"
      },
      "ticket": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "ClusterPhase": {
    "enum": [
      "Initializing",
      "Ready",
      "Unavailable",
      "Draining"
    ],
    "type": "string"
  },
  "ClusterSpec": {
    "properties": {
      "account": {
        "type": "string"
      },
      "running": {
        "type": "boolean"
      }
    },
    "required": [
      "account"
    ],
    "type": "object"
  },
  "ClusterStatus": {
    "properties": {
      "URL": {
        "type": "string"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "instance": {
        "type": "string"
      },
      "nodeExpiry": {
        "type": "integer"
      },
      "phase": {
        "ref": "ClusterPhase"
      }
    },
    "required": [],
    "type": "object"
  },
  "ComputeContract": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "ComputeContractSpec"
      },
      "status": {
        "ref": "ComputeContractStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "ComputeContractMeshd": {
    "properties": {
      "downloadBase": {
        "type": "string"
      },
      "pins": {
        "items": {
          "ref": "ComputeContractMeshdPin"
        },
        "type": "array"
      }
    },
    "required": [
      "downloadBase",
      "pins"
    ],
    "type": "object"
  },
  "ComputeContractMeshdPin": {
    "properties": {
      "arch": {
        "type": "string"
      },
      "sha256": {
        "type": "string"
      }
    },
    "required": [
      "arch",
      "sha256"
    ],
    "type": "object"
  },
  "ComputeContractNodeJoin": {
    "properties": {
      "cluster": {
        "type": "string"
      },
      "meshd": {
        "ref": "ComputeContractMeshd"
      }
    },
    "required": [
      "cluster"
    ],
    "type": "object"
  },
  "ComputeContractPhase": {
    "enum": [
      "Pending",
      "Requesting",
      "Held",
      "Releasing",
      "Released",
      "Failed"
    ],
    "type": "string"
  },
  "ComputeContractSpec": {
    "properties": {
      "adopt": {
        "type": "boolean"
      },
      "bidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "denyBidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "nodeJoin": {
        "ref": "ComputeContractNodeJoin"
      },
      "ownerDid": {
        "type": "string"
      },
      "policy": {
        "type": "string"
      },
      "policyArgs": {
        "type": "string"
      },
      "release": {
        "type": "boolean"
      },
      "secretsRef": {
        "ref": "VaultRef"
      },
      "sshAuthorizedKeys": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "userData": {
        "type": "string"
      },
      "vmName": {
        "type": "string"
      },
      "vmReadyTimeoutSec": {
        "type": "integer"
      }
    },
    "required": [],
    "type": "object"
  },
  "ComputeContractStatus": {
    "properties": {
      "bids": {
        "items": {
          "ref": "ContractBid"
        },
        "type": "array"
      },
      "chosen": {
        "ref": "ContractChosen"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "contract": {
        "ref": "NodeClaimContract"
      },
      "execution": {
        "ref": "ExecutionStatus"
      },
      "grant": {
        "ref": "NodeClaimGrant"
      },
      "message": {
        "type": "string"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "phase": {
        "ref": "ComputeContractPhase"
      },
      "secretsFetchedAt": {
        "type": "string"
      },
      "secretsUrl": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "Condition": {
    "open": true,
    "type": "object"
  },
  "ContractAcceptPending": {
    "properties": {
      "bidUri": {
        "type": "string"
      }
    },
    "required": [
      "bidUri"
    ],
    "type": "object"
  },
  "ContractBid": {
    "properties": {
      "arrivedAt": {
        "type": "string"
      },
      "cost": {
        "type": "integer"
      },
      "did": {
        "type": "string"
      },
      "offer": {
        "type": "string"
      },
      "uri": {
        "type": "string"
      },
      "verdict": {
        "ref": "ContractBidVerdict"
      }
    },
    "required": [
      "did",
      "uri",
      "verdict",
      "arrivedAt"
    ],
    "type": "object"
  },
  "ContractBidVerdict": {
    "enum": [
      "considered",
      "denied",
      "chosen",
      "notChosen",
      "policyRejected"
    ],
    "type": "string"
  },
  "ContractChosen": {
    "properties": {
      "considered": {
        "type": "integer"
      },
      "did": {
        "type": "string"
      },
      "reason": {
        "ref": "ContractChosenReason"
      },
      "uri": {
        "type": "string"
      }
    },
    "required": [
      "uri",
      "did",
      "reason",
      "considered"
    ],
    "type": "object"
  },
  "ContractChosenReason": {
    "enum": [
      "firstFree",
      "bestWithinWindow",
      "policy",
      "resumed"
    ],
    "type": "string"
  },
  "ContractHostKey": {
    "properties": {
      "fingerprint": {
        "type": "string"
      },
      "publicKey": {
        "type": "string"
      },
      "type": {
        "type": "string"
      }
    },
    "required": [
      "type",
      "publicKey",
      "fingerprint"
    ],
    "type": "object"
  },
  "ContractHostKeys": {
    "properties": {
      "event": {
        "ref": "RecordRef"
      },
      "keys": {
        "items": {
          "ref": "ContractHostKey"
        },
        "type": "array"
      },
      "observedAt": {
        "type": "string"
      },
      "payload": {
        "ref": "RecordRef"
      }
    },
    "required": [
      "keys",
      "observedAt",
      "event",
      "payload"
    ],
    "type": "object"
  },
  "ContractOutcome": {
    "enum": [
      "no_bids",
      "bids_denied",
      "policy_rejected",
      "returned",
      "receipt_invalid",
      "network_timeout",
      "complete"
    ],
    "type": "string"
  },
  "ContractPhase": {
    "enum": [
      "requested",
      "rfp_submitted",
      "winner_selected",
      "accepted",
      "receipt",
      "network",
      "released"
    ],
    "type": "string"
  },
  "ContractRelease": {
    "properties": {
      "event": {
        "ref": "RecordRef"
      },
      "ok": {
        "type": "boolean"
      },
      "payload": {
        "ref": "RecordRef"
      },
      "status": {
        "type": "integer"
      }
    },
    "required": [
      "event",
      "payload",
      "ok"
    ],
    "type": "object"
  },
  "ContractTermination": {
    "properties": {
      "event": {
        "ref": "RecordRef"
      },
      "exitCode": {
        "type": "integer"
      },
      "observedAt": {
        "type": "string"
      },
      "payload": {
        "ref": "RecordRef"
      },
      "reason": {
        "type": "string"
      },
      "request": {
        "ref": "RecordRef"
      }
    },
    "required": [
      "reason",
      "observedAt",
      "event",
      "payload"
    ],
    "type": "object"
  },
  "ContractTimestamps": {
    "properties": {
      "accepted": {
        "type": "string"
      },
      "network": {
        "type": "string"
      },
      "receipt": {
        "type": "string"
      },
      "released": {
        "type": "string"
      },
      "requested": {
        "type": "string"
      },
      "rfp_submitted": {
        "type": "string"
      },
      "winner_selected": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "ContractWinner": {
    "properties": {
      "did": {
        "type": "string"
      },
      "submitAccept": {
        "type": "string"
      }
    },
    "required": [
      "did"
    ],
    "type": "object"
  },
  "ExecutionStatus": {
    "properties": {
      "attempts": {
        "type": "integer"
      },
      "lastError": {
        "type": "string"
      },
      "lastEventAt": {
        "type": "string"
      },
      "step": {
        "type": "string"
      },
      "stepStartedAt": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "FirecrackerProvider": {
    "properties": {
      "vmruntimeCommand": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "workRoot": {
        "type": "string"
      }
    },
    "required": [
      "workRoot",
      "vmruntimeCommand"
    ],
    "type": "object"
  },
  "GCKindCount": {
    "properties": {
      "count": {
        "type": "integer"
      },
      "examples": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "kind": {
        "type": "string"
      }
    },
    "required": [
      "kind",
      "count"
    ],
    "type": "object"
  },
  "GCStatus": {
    "properties": {
      "objects": {
        "type": "integer"
      },
      "orphanTotal": {
        "type": "integer"
      },
      "orphans": {
        "items": {
          "ref": "GCKindCount"
        },
        "type": "array"
      },
      "sampledAt": {
        "type": "string"
      },
      "stuckTotal": {
        "type": "integer"
      },
      "terminating": {
        "items": {
          "ref": "GCStuck"
        },
        "type": "array"
      }
    },
    "required": [
      "sampledAt",
      "objects",
      "orphanTotal",
      "stuckTotal"
    ],
    "type": "object"
  },
  "GCStuck": {
    "properties": {
      "controller": {
        "type": "string"
      },
      "count": {
        "type": "integer"
      },
      "examples": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "finalizer": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      }
    },
    "required": [
      "kind",
      "finalizer",
      "controller",
      "count"
    ],
    "type": "object"
  },
  "Gateway": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "GatewaySpec"
      },
      "status": {
        "ref": "ApplicationStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "GatewayClusterProvider": {
    "properties": {
      "scaleHold": {
        "type": "string"
      },
      "scaleMaxNodes": {
        "type": "integer"
      },
      "scalePodsPerNode": {
        "type": "integer"
      }
    },
    "required": [],
    "type": "object"
  },
  "GatewayMarketNetwork": {
    "properties": {
      "nodeGuestHostAliases": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "nodeUserDataBaseFile": {
        "type": "string"
      },
      "ownerSessionsFile": {
        "type": "string"
      }
    },
    "required": [],
    "type": "object"
  },
  "GatewaySpec": {
    "properties": {
      "bidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "clusterProvider": {
        "ref": "GatewayClusterProvider"
      },
      "marketNetwork": {
        "ref": "GatewayMarketNetwork"
      },
      "network": {
        "ref": "MarketNetworkValue"
      },
      "operator": {
        "type": "string"
      },
      "options": {
        "items": {
          "ref": "ApplicationOption"
        },
        "type": "array"
      },
      "profile": {
        "type": "string"
      },
      "quota": {
        "ref": "ApplicationQuota"
      },
      "webAppFront": {
        "type": "string"
      }
    },
    "required": [
      "profile"
    ],
    "type": "object"
  },
  "IdentityScope": {
    "enum": [
      "Account",
      "Cluster"
    ],
    "type": "string"
  },
  "IdentityState": {
    "enum": [
      "Active",
      "Revoked",
      "RevocationPending",
      "ColdDeferred"
    ],
    "type": "string"
  },
  "KcpProvider": {
    "properties": {
      "guestImageSource": {
        "type": "string"
      },
      "handle": {
        "type": "string"
      },
      "stopGraceSeconds": {
        "type": "integer"
      },
      "vmHostBackend": {
        "type": "string"
      }
    },
    "required": [
      "guestImageSource"
    ],
    "type": "object"
  },
  "MarketBidder": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "MarketBidderSpec"
      },
      "status": {
        "ref": "ApplicationStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketBidderSpec": {
    "properties": {
      "firecracker": {
        "ref": "FirecrackerProvider"
      },
      "identity": {
        "ref": "ApplicationIdentity"
      },
      "kcp": {
        "ref": "KcpProvider"
      },
      "network": {
        "ref": "MarketNetworkValue"
      },
      "operator": {
        "type": "string"
      },
      "options": {
        "items": {
          "ref": "ApplicationOption"
        },
        "type": "array"
      },
      "policy": {
        "type": "string"
      },
      "profile": {
        "type": "string"
      },
      "quota": {
        "ref": "ApplicationQuota"
      },
      "signingKeyRotation": {
        "type": "integer"
      }
    },
    "required": [
      "profile",
      "identity"
    ],
    "type": "object"
  },
  "MarketDecline": {
    "properties": {
      "at": {
        "type": "string"
      },
      "reason": {
        "type": "string"
      },
      "rfpUri": {
        "type": "string"
      }
    },
    "required": [
      "rfpUri",
      "reason",
      "at"
    ],
    "type": "object"
  },
  "MarketDeclineCount": {
    "properties": {
      "count": {
        "type": "integer"
      },
      "reason": {
        "type": "string"
      }
    },
    "required": [
      "reason",
      "count"
    ],
    "type": "object"
  },
  "MarketIdentity": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "MarketIdentitySpec"
      },
      "status": {
        "ref": "MarketIdentityStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketIdentityMode": {
    "enum": [
      "oauthQr",
      "oauthBrowser",
      "pdsAccount",
      "localRepo",
      "oauthDelegated"
    ],
    "type": "string"
  },
  "MarketIdentityPendingAction": {
    "properties": {
      "callerDid": {
        "type": "string"
      },
      "expiresAt": {
        "type": "string"
      },
      "handle": {
        "type": "string"
      },
      "kind": {
        "ref": "PendingActionKind"
      },
      "url": {
        "type": "string"
      }
    },
    "required": [
      "kind"
    ],
    "type": "object"
  },
  "MarketIdentityPhase": {
    "enum": [
      "Pending",
      "AwaitingScan",
      "AwaitingAuthorization",
      "AwaitingApproval",
      "Ready",
      "SessionExpired",
      "Failed"
    ],
    "type": "string"
  },
  "MarketIdentitySpec": {
    "properties": {
      "approvedAssociations": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "associateWith": {
        "type": "string"
      },
      "association": {
        "ref": "AssociationMode"
      },
      "associationWatch": {
        "type": "string"
      },
      "handle": {
        "type": "string"
      },
      "keysRef": {
        "ref": "VaultRef"
      },
      "mode": {
        "ref": "MarketIdentityMode"
      },
      "pdsUrl": {
        "type": "string"
      },
      "rejectedAssociations": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "sessionRef": {
        "ref": "VaultRef"
      }
    },
    "required": [
      "mode"
    ],
    "type": "object"
  },
  "MarketIdentityStatus": {
    "properties": {
      "associations": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "did": {
        "type": "string"
      },
      "handle": {
        "type": "string"
      },
      "message": {
        "type": "string"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "pdsUrl": {
        "type": "string"
      },
      "pendingAction": {
        "ref": "MarketIdentityPendingAction"
      },
      "phase": {
        "ref": "MarketIdentityPhase"
      },
      "serviceDid": {
        "type": "string"
      },
      "sessionRef": {
        "ref": "VaultRef"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketLatencyStatus": {
    "properties": {
      "count": {
        "type": "integer"
      },
      "maxUs": {
        "type": "integer"
      },
      "p50Us": {
        "type": "integer"
      },
      "p95Us": {
        "type": "integer"
      }
    },
    "required": [
      "count",
      "p50Us",
      "p95Us",
      "maxUs"
    ],
    "type": "object"
  },
  "MarketLoopStatus": {
    "properties": {
      "bids": {
        "type": "integer"
      },
      "contractsActive": {
        "type": "integer"
      },
      "declinedByReason": {
        "items": {
          "ref": "MarketDeclineCount"
        },
        "type": "array"
      },
      "kcpWait": {
        "ref": "MarketLatencyStatus"
      },
      "pdsWrites": {
        "ref": "PdsWriteStatus"
      },
      "recentDeclines": {
        "items": {
          "ref": "MarketDecline"
        },
        "type": "array"
      },
      "rfpToBid": {
        "ref": "MarketLatencyStatus"
      },
      "rfpsScoped": {
        "type": "integer"
      },
      "rfpsSeen": {
        "type": "integer"
      },
      "streams": {
        "items": {
          "ref": "MarketStreamStatus"
        },
        "type": "array"
      },
      "updatedAt": {
        "type": "string"
      },
      "wins": {
        "type": "integer"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketNetwork": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "MarketNetworkSpec"
      },
      "status": {
        "ref": "MarketNetworkStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketNetworkSpec": {
    "properties": {
      "apps": {
        "items": {
          "ref": "NetworkApp"
        },
        "type": "array"
      },
      "guestLabel": {
        "type": "string"
      },
      "profile": {
        "type": "string"
      }
    },
    "required": [
      "profile"
    ],
    "type": "object"
  },
  "MarketNetworkStatus": {
    "properties": {
      "apps": {
        "items": {
          "ref": "NetworkAppStatus"
        },
        "type": "array"
      },
      "bidderPdsUrl": {
        "type": "string"
      },
      "components": {
        "items": {
          "ref": "ApplicationComponent"
        },
        "type": "array"
      },
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "gc": {
        "ref": "GCStatus"
      },
      "listeners": {
        "items": {
          "ref": "ApplicationListener"
        },
        "type": "array"
      },
      "message": {
        "type": "string"
      },
      "network": {
        "ref": "MarketNetworkValue"
      },
      "observedGeneration": {
        "type": "integer"
      },
      "phase": {
        "ref": "ApplicationPhase"
      },
      "stateDir": {
        "ref": "StateDirUsage"
      },
      "ttl": {
        "ref": "TTLStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketNetworkValue": {
    "properties": {
      "caBundleRef": {
        "type": "string"
      },
      "firehoseUrls": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "guestTlsUrl": {
        "type": "string"
      },
      "hosts": {
        "items": {
          "ref": "NetworkHost"
        },
        "type": "array"
      },
      "ingressProxyHost": {
        "type": "string"
      },
      "plcDirectoryUrl": {
        "type": "string"
      },
      "qrOrigin": {
        "type": "string"
      },
      "relayUrls": {
        "items": {
          "type": "string"
        },
        "type": "array"
      }
    },
    "required": [
      "plcDirectoryUrl",
      "ingressProxyHost",
      "relayUrls",
      "firehoseUrls",
      "qrOrigin"
    ],
    "type": "object"
  },
  "MarketRequester": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "MarketRequesterSpec"
      },
      "status": {
        "ref": "ApplicationStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "MarketRequesterSpec": {
    "properties": {
      "bidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "identity": {
        "ref": "ApplicationIdentity"
      },
      "network": {
        "ref": "MarketNetworkValue"
      },
      "options": {
        "items": {
          "ref": "ApplicationOption"
        },
        "type": "array"
      },
      "profile": {
        "type": "string"
      },
      "quota": {
        "ref": "ApplicationQuota"
      }
    },
    "required": [
      "profile",
      "identity"
    ],
    "type": "object"
  },
  "MarketStreamStatus": {
    "properties": {
      "cursor": {
        "type": "integer"
      },
      "lagMs": {
        "type": "integer"
      },
      "lastEventAt": {
        "type": "string"
      },
      "source": {
        "type": "string"
      }
    },
    "required": [
      "source"
    ],
    "type": "object"
  },
  "MissingReferencePolicy": {
    "enum": [
      "Keep",
      "Delete"
    ],
    "type": "string"
  },
  "NetworkApp": {
    "properties": {
      "clusterProvider": {
        "ref": "GatewayClusterProvider"
      },
      "firecracker": {
        "ref": "FirecrackerProvider"
      },
      "kcp": {
        "ref": "KcpProvider"
      },
      "kind": {
        "ref": "ApplicationKind"
      },
      "name": {
        "type": "string"
      },
      "options": {
        "items": {
          "ref": "ApplicationOption"
        },
        "type": "array"
      },
      "ownerSessionsFile": {
        "type": "string"
      },
      "policy": {
        "type": "string"
      },
      "profile": {
        "type": "string"
      },
      "quota": {
        "ref": "ApplicationQuota"
      },
      "stopped": {
        "type": "boolean"
      }
    },
    "required": [
      "name",
      "kind",
      "profile"
    ],
    "type": "object"
  },
  "NetworkAppStatus": {
    "properties": {
      "components": {
        "items": {
          "ref": "ApplicationComponent"
        },
        "type": "array"
      },
      "did": {
        "type": "string"
      },
      "instance": {
        "type": "string"
      },
      "kcpRoot": {
        "type": "string"
      },
      "kcpUrl": {
        "type": "string"
      },
      "kind": {
        "ref": "ApplicationKind"
      },
      "listeners": {
        "items": {
          "ref": "ApplicationListener"
        },
        "type": "array"
      },
      "message": {
        "type": "string"
      },
      "name": {
        "type": "string"
      },
      "phase": {
        "ref": "ApplicationPhase"
      }
    },
    "required": [
      "name",
      "kind"
    ],
    "type": "object"
  },
  "NetworkHost": {
    "properties": {
      "address": {
        "type": "string"
      },
      "name": {
        "type": "string"
      }
    },
    "required": [
      "name",
      "address"
    ],
    "type": "object"
  },
  "Node": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "NodeSpec"
      },
      "status": {
        "ref": "NodeStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "NodeClaim": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "NodeClaimSpec"
      },
      "status": {
        "ref": "NodeClaimStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "NodeClaimContract": {
    "properties": {
      "accept": {
        "ref": "RecordRef"
      },
      "acceptPending": {
        "ref": "ContractAcceptPending"
      },
      "acceptUri": {
        "type": "string"
      },
      "bid": {
        "ref": "RecordRef"
      },
      "bids": {
        "type": "integer"
      },
      "error": {
        "type": "string"
      },
      "hostKeys": {
        "ref": "ContractHostKeys"
      },
      "marketDid": {
        "type": "string"
      },
      "outcome": {
        "ref": "ContractOutcome"
      },
      "payload": {
        "ref": "RecordRef"
      },
      "phase": {
        "ref": "ContractPhase"
      },
      "policy": {
        "ref": "RecordRef"
      },
      "receipt": {
        "ref": "RecordRef"
      },
      "receiptCid": {
        "type": "string"
      },
      "receiptOk": {
        "type": "boolean"
      },
      "receiptUri": {
        "type": "string"
      },
      "release": {
        "ref": "ContractRelease"
      },
      "requesterDid": {
        "type": "string"
      },
      "rfp": {
        "ref": "RecordRef"
      },
      "rfpUri": {
        "type": "string"
      },
      "submitEventRef": {
        "type": "string"
      },
      "termination": {
        "ref": "ContractTermination"
      },
      "timestamps": {
        "ref": "ContractTimestamps"
      },
      "version": {
        "type": "integer"
      },
      "vmAddress": {
        "type": "string"
      },
      "vmName": {
        "type": "string"
      },
      "vmUri": {
        "type": "string"
      },
      "winner": {
        "ref": "ContractWinner"
      }
    },
    "required": [],
    "type": "object"
  },
  "NodeClaimGrant": {
    "properties": {
      "audience": {
        "type": "string"
      },
      "issuer": {
        "type": "string"
      },
      "role": {
        "type": "string"
      },
      "subject": {
        "type": "string"
      }
    },
    "required": [
      "issuer",
      "subject",
      "audience"
    ],
    "type": "object"
  },
  "NodeClaimPhase": {
    "enum": [
      "Pending",
      "Bidding",
      "Provisioning",
      "Joining",
      "Running",
      "Failed",
      "Released",
      "Lost",
      "Adopting"
    ],
    "type": "string"
  },
  "NodeClaimSecrets": {
    "properties": {
      "ingressDid": {
        "type": "string"
      },
      "keyRef": {
        "type": "string"
      },
      "servedAt": {
        "type": "string"
      },
      "url": {
        "type": "string"
      }
    },
    "required": [
      "ingressDid",
      "keyRef",
      "url"
    ],
    "type": "object"
  },
  "NodeClaimSpec": {
    "properties": {
      "advertiseAddress": {
        "type": "string"
      },
      "apiserverPort": {
        "type": "integer"
      },
      "cluster": {
        "type": "string"
      },
      "denyBidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "nodeName": {
        "type": "string"
      },
      "nodePasswordFile": {
        "type": "string"
      },
      "ownerDid": {
        "type": "string"
      },
      "serverCAFile": {
        "type": "string"
      },
      "tokenFile": {
        "type": "string"
      }
    },
    "required": [
      "cluster",
      "nodeName",
      "ownerDid",
      "apiserverPort",
      "advertiseAddress",
      "tokenFile",
      "nodePasswordFile",
      "serverCAFile"
    ],
    "type": "object"
  },
  "NodeClaimStatus": {
    "properties": {
      "contract": {
        "ref": "NodeClaimContract"
      },
      "fqdn": {
        "type": "string"
      },
      "grant": {
        "ref": "NodeClaimGrant"
      },
      "message": {
        "type": "string"
      },
      "phase": {
        "ref": "NodeClaimPhase"
      },
      "secrets": {
        "ref": "NodeClaimSecrets"
      }
    },
    "required": [],
    "type": "object"
  },
  "NodePhase": {
    "enum": [
      "Pending",
      "Running",
      "Draining"
    ],
    "type": "string"
  },
  "NodeSpec": {
    "properties": {
      "cluster": {
        "type": "string"
      }
    },
    "required": [
      "cluster"
    ],
    "type": "object"
  },
  "NodeStatus": {
    "properties": {
      "conditions": {
        "items": {
          "ref": "Condition"
        },
        "type": "array"
      },
      "nodeExpiry": {
        "type": "integer"
      },
      "phase": {
        "ref": "NodePhase"
      }
    },
    "required": [],
    "type": "object"
  },
  "ObjectMeta": {
    "open": true,
    "type": "object"
  },
  "PdsLimitStatus": {
    "properties": {
      "limit": {
        "type": "integer"
      },
      "observedAt": {
        "type": "string"
      },
      "pausedUntil": {
        "type": "string"
      },
      "pds": {
        "type": "string"
      },
      "remaining": {
        "type": "integer"
      },
      "resetAt": {
        "type": "string"
      }
    },
    "required": [
      "pds",
      "observedAt"
    ],
    "type": "object"
  },
  "PdsWriteCollection": {
    "properties": {
      "collection": {
        "type": "string"
      },
      "points": {
        "type": "integer"
      },
      "writes": {
        "type": "integer"
      }
    },
    "required": [
      "collection",
      "writes",
      "points"
    ],
    "type": "object"
  },
  "PdsWriteHour": {
    "properties": {
      "hour": {
        "type": "string"
      },
      "points": {
        "type": "integer"
      }
    },
    "required": [
      "hour",
      "points"
    ],
    "type": "object"
  },
  "PdsWriteStatus": {
    "properties": {
      "byCollection": {
        "items": {
          "ref": "PdsWriteCollection"
        },
        "type": "array"
      },
      "limits": {
        "items": {
          "ref": "PdsLimitStatus"
        },
        "type": "array"
      },
      "perHour": {
        "items": {
          "ref": "PdsWriteHour"
        },
        "type": "array"
      },
      "points": {
        "type": "integer"
      },
      "rateLimited": {
        "type": "integer"
      },
      "reserve": {
        "type": "integer"
      },
      "since": {
        "type": "string"
      },
      "writes": {
        "type": "integer"
      }
    },
    "required": [
      "since",
      "writes",
      "points",
      "reserve"
    ],
    "type": "object"
  },
  "PendingActionKind": {
    "enum": [
      "scanQr",
      "visitUrl",
      "approveAssociation",
      "signInAgain"
    ],
    "type": "string"
  },
  "QuotaUsage": {
    "properties": {
      "hard": {
        "type": "integer"
      },
      "resource": {
        "type": "string"
      },
      "used": {
        "type": "integer"
      },
      "workspace": {
        "type": "string"
      }
    },
    "required": [
      "workspace",
      "resource",
      "hard",
      "used"
    ],
    "type": "object"
  },
  "RecordRef": {
    "properties": {
      "cid": {
        "type": "string"
      },
      "uri": {
        "type": "string"
      }
    },
    "required": [
      "uri",
      "cid"
    ],
    "type": "object"
  },
  "SshGateway": {
    "properties": {
      "apiVersion": {
        "type": "string"
      },
      "kind": {
        "type": "string"
      },
      "metadata": {
        "ref": "ObjectMeta"
      },
      "spec": {
        "ref": "SshGatewaySpec"
      },
      "status": {
        "ref": "ApplicationStatus"
      }
    },
    "required": [],
    "type": "object"
  },
  "SshGatewaySpec": {
    "properties": {
      "bidderDids": {
        "items": {
          "type": "string"
        },
        "type": "array"
      },
      "network": {
        "ref": "MarketNetworkValue"
      },
      "options": {
        "items": {
          "ref": "ApplicationOption"
        },
        "type": "array"
      },
      "ownerSessionsFile": {
        "type": "string"
      },
      "profile": {
        "type": "string"
      },
      "quota": {
        "ref": "ApplicationQuota"
      }
    },
    "required": [
      "profile"
    ],
    "type": "object"
  },
  "StateDirUsage": {
    "properties": {
      "bytes": {
        "type": "integer"
      },
      "files": {
        "type": "integer"
      },
      "path": {
        "type": "string"
      },
      "sampledAt": {
        "type": "string"
      }
    },
    "required": [
      "path",
      "bytes",
      "files",
      "sampledAt"
    ],
    "type": "object"
  },
  "StoreUsage": {
    "properties": {
      "autoVacuum": {
        "type": "string"
      },
      "bytes": {
        "type": "integer"
      },
      "deadTuples": {
        "type": "integer"
      },
      "freeBytes": {
        "type": "integer"
      },
      "kind": {
        "type": "string"
      },
      "lastVacuumAt": {
        "type": "string"
      },
      "path": {
        "type": "string"
      },
      "reclaimedBytes": {
        "type": "integer"
      },
      "rows": {
        "type": "integer"
      },
      "sampledAt": {
        "type": "string"
      },
      "vacuums": {
        "type": "integer"
      }
    },
    "required": [
      "kind",
      "bytes",
      "sampledAt"
    ],
    "type": "object"
  },
  "TTLDeleted": {
    "properties": {
      "count": {
        "type": "integer"
      },
      "kind": {
        "type": "string"
      },
      "lastAt": {
        "type": "string"
      }
    },
    "required": [
      "kind",
      "count"
    ],
    "type": "object"
  },
  "TTLStatus": {
    "properties": {
      "deleted": {
        "items": {
          "ref": "TTLDeleted"
        },
        "type": "array"
      },
      "deletedTotal": {
        "type": "integer"
      },
      "sampledAt": {
        "type": "string"
      },
      "tracked": {
        "type": "integer"
      }
    },
    "required": [
      "sampledAt",
      "tracked",
      "deletedTotal"
    ],
    "type": "object"
  },
  "VaultRef": {
    "properties": {
      "mount": {
        "type": "string"
      },
      "path": {
        "type": "string"
      }
    },
    "required": [
      "mount",
      "path"
    ],
    "type": "object"
  },
  "WatchedRecord": {
    "properties": {
      "cid": {
        "type": "string"
      },
      "did": {
        "type": "string"
      },
      "handle": {
        "type": "string"
      },
      "uri": {
        "type": "string"
      },
      "value": {
        "open": true,
        "type": "object"
      },
      "verifiedAt": {
        "type": "string"
      }
    },
    "required": [
      "did",
      "uri",
      "value",
      "verifiedAt"
    ],
    "type": "object"
  }
};

export function validateCluster(value: unknown): ApiIssue[] {
  return validate("Cluster", value);
}

export function isCluster(value: unknown): value is Cluster {
  return validate("Cluster", value).length === 0;
}

export function assertCluster(value: unknown): asserts value is Cluster {
  const issues = validate("Cluster", value);
  if (issues.length > 0) throw new ApiValidationError("Cluster", issues);
}

export function parseCluster(value: unknown): Cluster {
  assertCluster(value);
  return value;
}

export function validateClusterSpec(value: unknown): ApiIssue[] {
  return validate("ClusterSpec", value);
}

export function isClusterSpec(value: unknown): value is ClusterSpec {
  return validate("ClusterSpec", value).length === 0;
}

export function assertClusterSpec(value: unknown): asserts value is ClusterSpec {
  const issues = validate("ClusterSpec", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterSpec", issues);
}

export function parseClusterSpec(value: unknown): ClusterSpec {
  assertClusterSpec(value);
  return value;
}

export function validateClusterStatus(value: unknown): ApiIssue[] {
  return validate("ClusterStatus", value);
}

export function isClusterStatus(value: unknown): value is ClusterStatus {
  return validate("ClusterStatus", value).length === 0;
}

export function assertClusterStatus(value: unknown): asserts value is ClusterStatus {
  const issues = validate("ClusterStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterStatus", issues);
}

export function parseClusterStatus(value: unknown): ClusterStatus {
  assertClusterStatus(value);
  return value;
}

export function validateClusterPhase(value: unknown): ApiIssue[] {
  return validate("ClusterPhase", value);
}

export function isClusterPhase(value: unknown): value is ClusterPhase {
  return validate("ClusterPhase", value).length === 0;
}

export function assertClusterPhase(value: unknown): asserts value is ClusterPhase {
  const issues = validate("ClusterPhase", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterPhase", issues);
}

export function parseClusterPhase(value: unknown): ClusterPhase {
  assertClusterPhase(value);
  return value;
}

export function validateNode(value: unknown): ApiIssue[] {
  return validate("Node", value);
}

export function isNode(value: unknown): value is Node {
  return validate("Node", value).length === 0;
}

export function assertNode(value: unknown): asserts value is Node {
  const issues = validate("Node", value);
  if (issues.length > 0) throw new ApiValidationError("Node", issues);
}

export function parseNode(value: unknown): Node {
  assertNode(value);
  return value;
}

export function validateNodeSpec(value: unknown): ApiIssue[] {
  return validate("NodeSpec", value);
}

export function isNodeSpec(value: unknown): value is NodeSpec {
  return validate("NodeSpec", value).length === 0;
}

export function assertNodeSpec(value: unknown): asserts value is NodeSpec {
  const issues = validate("NodeSpec", value);
  if (issues.length > 0) throw new ApiValidationError("NodeSpec", issues);
}

export function parseNodeSpec(value: unknown): NodeSpec {
  assertNodeSpec(value);
  return value;
}

export function validateNodeStatus(value: unknown): ApiIssue[] {
  return validate("NodeStatus", value);
}

export function isNodeStatus(value: unknown): value is NodeStatus {
  return validate("NodeStatus", value).length === 0;
}

export function assertNodeStatus(value: unknown): asserts value is NodeStatus {
  const issues = validate("NodeStatus", value);
  if (issues.length > 0) throw new ApiValidationError("NodeStatus", issues);
}

export function parseNodeStatus(value: unknown): NodeStatus {
  assertNodeStatus(value);
  return value;
}

export function validateNodePhase(value: unknown): ApiIssue[] {
  return validate("NodePhase", value);
}

export function isNodePhase(value: unknown): value is NodePhase {
  return validate("NodePhase", value).length === 0;
}

export function assertNodePhase(value: unknown): asserts value is NodePhase {
  const issues = validate("NodePhase", value);
  if (issues.length > 0) throw new ApiValidationError("NodePhase", issues);
}

export function parseNodePhase(value: unknown): NodePhase {
  assertNodePhase(value);
  return value;
}

export function validateNodeClaim(value: unknown): ApiIssue[] {
  return validate("NodeClaim", value);
}

export function isNodeClaim(value: unknown): value is NodeClaim {
  return validate("NodeClaim", value).length === 0;
}

export function assertNodeClaim(value: unknown): asserts value is NodeClaim {
  const issues = validate("NodeClaim", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaim", issues);
}

export function parseNodeClaim(value: unknown): NodeClaim {
  assertNodeClaim(value);
  return value;
}

export function validateNodeClaimSpec(value: unknown): ApiIssue[] {
  return validate("NodeClaimSpec", value);
}

export function isNodeClaimSpec(value: unknown): value is NodeClaimSpec {
  return validate("NodeClaimSpec", value).length === 0;
}

export function assertNodeClaimSpec(value: unknown): asserts value is NodeClaimSpec {
  const issues = validate("NodeClaimSpec", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimSpec", issues);
}

export function parseNodeClaimSpec(value: unknown): NodeClaimSpec {
  assertNodeClaimSpec(value);
  return value;
}

export function validateNodeClaimStatus(value: unknown): ApiIssue[] {
  return validate("NodeClaimStatus", value);
}

export function isNodeClaimStatus(value: unknown): value is NodeClaimStatus {
  return validate("NodeClaimStatus", value).length === 0;
}

export function assertNodeClaimStatus(value: unknown): asserts value is NodeClaimStatus {
  const issues = validate("NodeClaimStatus", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimStatus", issues);
}

export function parseNodeClaimStatus(value: unknown): NodeClaimStatus {
  assertNodeClaimStatus(value);
  return value;
}

export function validateNodeClaimPhase(value: unknown): ApiIssue[] {
  return validate("NodeClaimPhase", value);
}

export function isNodeClaimPhase(value: unknown): value is NodeClaimPhase {
  return validate("NodeClaimPhase", value).length === 0;
}

export function assertNodeClaimPhase(value: unknown): asserts value is NodeClaimPhase {
  const issues = validate("NodeClaimPhase", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimPhase", issues);
}

export function parseNodeClaimPhase(value: unknown): NodeClaimPhase {
  assertNodeClaimPhase(value);
  return value;
}

export function validateNodeClaimContract(value: unknown): ApiIssue[] {
  return validate("NodeClaimContract", value);
}

export function isNodeClaimContract(value: unknown): value is NodeClaimContract {
  return validate("NodeClaimContract", value).length === 0;
}

export function assertNodeClaimContract(value: unknown): asserts value is NodeClaimContract {
  const issues = validate("NodeClaimContract", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimContract", issues);
}

export function parseNodeClaimContract(value: unknown): NodeClaimContract {
  assertNodeClaimContract(value);
  return value;
}

export function validateContractPhase(value: unknown): ApiIssue[] {
  return validate("ContractPhase", value);
}

export function isContractPhase(value: unknown): value is ContractPhase {
  return validate("ContractPhase", value).length === 0;
}

export function assertContractPhase(value: unknown): asserts value is ContractPhase {
  const issues = validate("ContractPhase", value);
  if (issues.length > 0) throw new ApiValidationError("ContractPhase", issues);
}

export function parseContractPhase(value: unknown): ContractPhase {
  assertContractPhase(value);
  return value;
}

export function validateRecordRef(value: unknown): ApiIssue[] {
  return validate("RecordRef", value);
}

export function isRecordRef(value: unknown): value is RecordRef {
  return validate("RecordRef", value).length === 0;
}

export function assertRecordRef(value: unknown): asserts value is RecordRef {
  const issues = validate("RecordRef", value);
  if (issues.length > 0) throw new ApiValidationError("RecordRef", issues);
}

export function parseRecordRef(value: unknown): RecordRef {
  assertRecordRef(value);
  return value;
}

export function validateContractWinner(value: unknown): ApiIssue[] {
  return validate("ContractWinner", value);
}

export function isContractWinner(value: unknown): value is ContractWinner {
  return validate("ContractWinner", value).length === 0;
}

export function assertContractWinner(value: unknown): asserts value is ContractWinner {
  const issues = validate("ContractWinner", value);
  if (issues.length > 0) throw new ApiValidationError("ContractWinner", issues);
}

export function parseContractWinner(value: unknown): ContractWinner {
  assertContractWinner(value);
  return value;
}

export function validateContractAcceptPending(value: unknown): ApiIssue[] {
  return validate("ContractAcceptPending", value);
}

export function isContractAcceptPending(value: unknown): value is ContractAcceptPending {
  return validate("ContractAcceptPending", value).length === 0;
}

export function assertContractAcceptPending(value: unknown): asserts value is ContractAcceptPending {
  const issues = validate("ContractAcceptPending", value);
  if (issues.length > 0) throw new ApiValidationError("ContractAcceptPending", issues);
}

export function parseContractAcceptPending(value: unknown): ContractAcceptPending {
  assertContractAcceptPending(value);
  return value;
}

export function validateContractRelease(value: unknown): ApiIssue[] {
  return validate("ContractRelease", value);
}

export function isContractRelease(value: unknown): value is ContractRelease {
  return validate("ContractRelease", value).length === 0;
}

export function assertContractRelease(value: unknown): asserts value is ContractRelease {
  const issues = validate("ContractRelease", value);
  if (issues.length > 0) throw new ApiValidationError("ContractRelease", issues);
}

export function parseContractRelease(value: unknown): ContractRelease {
  assertContractRelease(value);
  return value;
}

export function validateContractTermination(value: unknown): ApiIssue[] {
  return validate("ContractTermination", value);
}

export function isContractTermination(value: unknown): value is ContractTermination {
  return validate("ContractTermination", value).length === 0;
}

export function assertContractTermination(value: unknown): asserts value is ContractTermination {
  const issues = validate("ContractTermination", value);
  if (issues.length > 0) throw new ApiValidationError("ContractTermination", issues);
}

export function parseContractTermination(value: unknown): ContractTermination {
  assertContractTermination(value);
  return value;
}

export function validateContractHostKeys(value: unknown): ApiIssue[] {
  return validate("ContractHostKeys", value);
}

export function isContractHostKeys(value: unknown): value is ContractHostKeys {
  return validate("ContractHostKeys", value).length === 0;
}

export function assertContractHostKeys(value: unknown): asserts value is ContractHostKeys {
  const issues = validate("ContractHostKeys", value);
  if (issues.length > 0) throw new ApiValidationError("ContractHostKeys", issues);
}

export function parseContractHostKeys(value: unknown): ContractHostKeys {
  assertContractHostKeys(value);
  return value;
}

export function validateContractHostKey(value: unknown): ApiIssue[] {
  return validate("ContractHostKey", value);
}

export function isContractHostKey(value: unknown): value is ContractHostKey {
  return validate("ContractHostKey", value).length === 0;
}

export function assertContractHostKey(value: unknown): asserts value is ContractHostKey {
  const issues = validate("ContractHostKey", value);
  if (issues.length > 0) throw new ApiValidationError("ContractHostKey", issues);
}

export function parseContractHostKey(value: unknown): ContractHostKey {
  assertContractHostKey(value);
  return value;
}

export function validateContractOutcome(value: unknown): ApiIssue[] {
  return validate("ContractOutcome", value);
}

export function isContractOutcome(value: unknown): value is ContractOutcome {
  return validate("ContractOutcome", value).length === 0;
}

export function assertContractOutcome(value: unknown): asserts value is ContractOutcome {
  const issues = validate("ContractOutcome", value);
  if (issues.length > 0) throw new ApiValidationError("ContractOutcome", issues);
}

export function parseContractOutcome(value: unknown): ContractOutcome {
  assertContractOutcome(value);
  return value;
}

export function validateContractTimestamps(value: unknown): ApiIssue[] {
  return validate("ContractTimestamps", value);
}

export function isContractTimestamps(value: unknown): value is ContractTimestamps {
  return validate("ContractTimestamps", value).length === 0;
}

export function assertContractTimestamps(value: unknown): asserts value is ContractTimestamps {
  const issues = validate("ContractTimestamps", value);
  if (issues.length > 0) throw new ApiValidationError("ContractTimestamps", issues);
}

export function parseContractTimestamps(value: unknown): ContractTimestamps {
  assertContractTimestamps(value);
  return value;
}

export function validateNodeClaimGrant(value: unknown): ApiIssue[] {
  return validate("NodeClaimGrant", value);
}

export function isNodeClaimGrant(value: unknown): value is NodeClaimGrant {
  return validate("NodeClaimGrant", value).length === 0;
}

export function assertNodeClaimGrant(value: unknown): asserts value is NodeClaimGrant {
  const issues = validate("NodeClaimGrant", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimGrant", issues);
}

export function parseNodeClaimGrant(value: unknown): NodeClaimGrant {
  assertNodeClaimGrant(value);
  return value;
}

export function validateNodeClaimSecrets(value: unknown): ApiIssue[] {
  return validate("NodeClaimSecrets", value);
}

export function isNodeClaimSecrets(value: unknown): value is NodeClaimSecrets {
  return validate("NodeClaimSecrets", value).length === 0;
}

export function assertNodeClaimSecrets(value: unknown): asserts value is NodeClaimSecrets {
  const issues = validate("NodeClaimSecrets", value);
  if (issues.length > 0) throw new ApiValidationError("NodeClaimSecrets", issues);
}

export function parseNodeClaimSecrets(value: unknown): NodeClaimSecrets {
  assertNodeClaimSecrets(value);
  return value;
}

export function validateClusterLink(value: unknown): ApiIssue[] {
  return validate("ClusterLink", value);
}

export function isClusterLink(value: unknown): value is ClusterLink {
  return validate("ClusterLink", value).length === 0;
}

export function assertClusterLink(value: unknown): asserts value is ClusterLink {
  const issues = validate("ClusterLink", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterLink", issues);
}

export function parseClusterLink(value: unknown): ClusterLink {
  assertClusterLink(value);
  return value;
}

export function validateClusterLinkSpec(value: unknown): ApiIssue[] {
  return validate("ClusterLinkSpec", value);
}

export function isClusterLinkSpec(value: unknown): value is ClusterLinkSpec {
  return validate("ClusterLinkSpec", value).length === 0;
}

export function assertClusterLinkSpec(value: unknown): asserts value is ClusterLinkSpec {
  const issues = validate("ClusterLinkSpec", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterLinkSpec", issues);
}

export function parseClusterLinkSpec(value: unknown): ClusterLinkSpec {
  assertClusterLinkSpec(value);
  return value;
}

export function validateClusterLinkService(value: unknown): ApiIssue[] {
  return validate("ClusterLinkService", value);
}

export function isClusterLinkService(value: unknown): value is ClusterLinkService {
  return validate("ClusterLinkService", value).length === 0;
}

export function assertClusterLinkService(value: unknown): asserts value is ClusterLinkService {
  const issues = validate("ClusterLinkService", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterLinkService", issues);
}

export function parseClusterLinkService(value: unknown): ClusterLinkService {
  assertClusterLinkService(value);
  return value;
}

export function validateClusterLinkStatus(value: unknown): ApiIssue[] {
  return validate("ClusterLinkStatus", value);
}

export function isClusterLinkStatus(value: unknown): value is ClusterLinkStatus {
  return validate("ClusterLinkStatus", value).length === 0;
}

export function assertClusterLinkStatus(value: unknown): asserts value is ClusterLinkStatus {
  const issues = validate("ClusterLinkStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterLinkStatus", issues);
}

export function parseClusterLinkStatus(value: unknown): ClusterLinkStatus {
  assertClusterLinkStatus(value);
  return value;
}

export function validateClusterLinkPhase(value: unknown): ApiIssue[] {
  return validate("ClusterLinkPhase", value);
}

export function isClusterLinkPhase(value: unknown): value is ClusterLinkPhase {
  return validate("ClusterLinkPhase", value).length === 0;
}

export function assertClusterLinkPhase(value: unknown): asserts value is ClusterLinkPhase {
  const issues = validate("ClusterLinkPhase", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterLinkPhase", issues);
}

export function parseClusterLinkPhase(value: unknown): ClusterLinkPhase {
  assertClusterLinkPhase(value);
  return value;
}

export function validateClusterClientCertificate(value: unknown): ApiIssue[] {
  return validate("ClusterClientCertificate", value);
}

export function isClusterClientCertificate(value: unknown): value is ClusterClientCertificate {
  return validate("ClusterClientCertificate", value).length === 0;
}

export function assertClusterClientCertificate(value: unknown): asserts value is ClusterClientCertificate {
  const issues = validate("ClusterClientCertificate", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterClientCertificate", issues);
}

export function parseClusterClientCertificate(value: unknown): ClusterClientCertificate {
  assertClusterClientCertificate(value);
  return value;
}

export function validateClusterClientCertificateSpec(value: unknown): ApiIssue[] {
  return validate("ClusterClientCertificateSpec", value);
}

export function isClusterClientCertificateSpec(value: unknown): value is ClusterClientCertificateSpec {
  return validate("ClusterClientCertificateSpec", value).length === 0;
}

export function assertClusterClientCertificateSpec(value: unknown): asserts value is ClusterClientCertificateSpec {
  const issues = validate("ClusterClientCertificateSpec", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterClientCertificateSpec", issues);
}

export function parseClusterClientCertificateSpec(value: unknown): ClusterClientCertificateSpec {
  assertClusterClientCertificateSpec(value);
  return value;
}

export function validateClusterClientCertificateStatus(value: unknown): ApiIssue[] {
  return validate("ClusterClientCertificateStatus", value);
}

export function isClusterClientCertificateStatus(value: unknown): value is ClusterClientCertificateStatus {
  return validate("ClusterClientCertificateStatus", value).length === 0;
}

export function assertClusterClientCertificateStatus(value: unknown): asserts value is ClusterClientCertificateStatus {
  const issues = validate("ClusterClientCertificateStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ClusterClientCertificateStatus", issues);
}

export function parseClusterClientCertificateStatus(value: unknown): ClusterClientCertificateStatus {
  assertClusterClientCertificateStatus(value);
  return value;
}

export function validateAtprotoIdentity(value: unknown): ApiIssue[] {
  return validate("AtprotoIdentity", value);
}

export function isAtprotoIdentity(value: unknown): value is AtprotoIdentity {
  return validate("AtprotoIdentity", value).length === 0;
}

export function assertAtprotoIdentity(value: unknown): asserts value is AtprotoIdentity {
  const issues = validate("AtprotoIdentity", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoIdentity", issues);
}

export function parseAtprotoIdentity(value: unknown): AtprotoIdentity {
  assertAtprotoIdentity(value);
  return value;
}

export function validateAtprotoIdentitySpec(value: unknown): ApiIssue[] {
  return validate("AtprotoIdentitySpec", value);
}

export function isAtprotoIdentitySpec(value: unknown): value is AtprotoIdentitySpec {
  return validate("AtprotoIdentitySpec", value).length === 0;
}

export function assertAtprotoIdentitySpec(value: unknown): asserts value is AtprotoIdentitySpec {
  const issues = validate("AtprotoIdentitySpec", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoIdentitySpec", issues);
}

export function parseAtprotoIdentitySpec(value: unknown): AtprotoIdentitySpec {
  assertAtprotoIdentitySpec(value);
  return value;
}

export function validateIdentityScope(value: unknown): ApiIssue[] {
  return validate("IdentityScope", value);
}

export function isIdentityScope(value: unknown): value is IdentityScope {
  return validate("IdentityScope", value).length === 0;
}

export function assertIdentityScope(value: unknown): asserts value is IdentityScope {
  const issues = validate("IdentityScope", value);
  if (issues.length > 0) throw new ApiValidationError("IdentityScope", issues);
}

export function parseIdentityScope(value: unknown): IdentityScope {
  assertIdentityScope(value);
  return value;
}

export function validateAtprotoIdentityStatus(value: unknown): ApiIssue[] {
  return validate("AtprotoIdentityStatus", value);
}

export function isAtprotoIdentityStatus(value: unknown): value is AtprotoIdentityStatus {
  return validate("AtprotoIdentityStatus", value).length === 0;
}

export function assertAtprotoIdentityStatus(value: unknown): asserts value is AtprotoIdentityStatus {
  const issues = validate("AtprotoIdentityStatus", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoIdentityStatus", issues);
}

export function parseAtprotoIdentityStatus(value: unknown): AtprotoIdentityStatus {
  assertAtprotoIdentityStatus(value);
  return value;
}

export function validateIdentityState(value: unknown): ApiIssue[] {
  return validate("IdentityState", value);
}

export function isIdentityState(value: unknown): value is IdentityState {
  return validate("IdentityState", value).length === 0;
}

export function assertIdentityState(value: unknown): asserts value is IdentityState {
  const issues = validate("IdentityState", value);
  if (issues.length > 0) throw new ApiValidationError("IdentityState", issues);
}

export function parseIdentityState(value: unknown): IdentityState {
  assertIdentityState(value);
  return value;
}

export function validateMarketIdentity(value: unknown): ApiIssue[] {
  return validate("MarketIdentity", value);
}

export function isMarketIdentity(value: unknown): value is MarketIdentity {
  return validate("MarketIdentity", value).length === 0;
}

export function assertMarketIdentity(value: unknown): asserts value is MarketIdentity {
  const issues = validate("MarketIdentity", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentity", issues);
}

export function parseMarketIdentity(value: unknown): MarketIdentity {
  assertMarketIdentity(value);
  return value;
}

export function validateMarketIdentitySpec(value: unknown): ApiIssue[] {
  return validate("MarketIdentitySpec", value);
}

export function isMarketIdentitySpec(value: unknown): value is MarketIdentitySpec {
  return validate("MarketIdentitySpec", value).length === 0;
}

export function assertMarketIdentitySpec(value: unknown): asserts value is MarketIdentitySpec {
  const issues = validate("MarketIdentitySpec", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentitySpec", issues);
}

export function parseMarketIdentitySpec(value: unknown): MarketIdentitySpec {
  assertMarketIdentitySpec(value);
  return value;
}

export function validateMarketIdentityMode(value: unknown): ApiIssue[] {
  return validate("MarketIdentityMode", value);
}

export function isMarketIdentityMode(value: unknown): value is MarketIdentityMode {
  return validate("MarketIdentityMode", value).length === 0;
}

export function assertMarketIdentityMode(value: unknown): asserts value is MarketIdentityMode {
  const issues = validate("MarketIdentityMode", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentityMode", issues);
}

export function parseMarketIdentityMode(value: unknown): MarketIdentityMode {
  assertMarketIdentityMode(value);
  return value;
}

export function validateAssociationMode(value: unknown): ApiIssue[] {
  return validate("AssociationMode", value);
}

export function isAssociationMode(value: unknown): value is AssociationMode {
  return validate("AssociationMode", value).length === 0;
}

export function assertAssociationMode(value: unknown): asserts value is AssociationMode {
  const issues = validate("AssociationMode", value);
  if (issues.length > 0) throw new ApiValidationError("AssociationMode", issues);
}

export function parseAssociationMode(value: unknown): AssociationMode {
  assertAssociationMode(value);
  return value;
}

export function validateVaultRef(value: unknown): ApiIssue[] {
  return validate("VaultRef", value);
}

export function isVaultRef(value: unknown): value is VaultRef {
  return validate("VaultRef", value).length === 0;
}

export function assertVaultRef(value: unknown): asserts value is VaultRef {
  const issues = validate("VaultRef", value);
  if (issues.length > 0) throw new ApiValidationError("VaultRef", issues);
}

export function parseVaultRef(value: unknown): VaultRef {
  assertVaultRef(value);
  return value;
}

export function validateMarketIdentityStatus(value: unknown): ApiIssue[] {
  return validate("MarketIdentityStatus", value);
}

export function isMarketIdentityStatus(value: unknown): value is MarketIdentityStatus {
  return validate("MarketIdentityStatus", value).length === 0;
}

export function assertMarketIdentityStatus(value: unknown): asserts value is MarketIdentityStatus {
  const issues = validate("MarketIdentityStatus", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentityStatus", issues);
}

export function parseMarketIdentityStatus(value: unknown): MarketIdentityStatus {
  assertMarketIdentityStatus(value);
  return value;
}

export function validateMarketIdentityPhase(value: unknown): ApiIssue[] {
  return validate("MarketIdentityPhase", value);
}

export function isMarketIdentityPhase(value: unknown): value is MarketIdentityPhase {
  return validate("MarketIdentityPhase", value).length === 0;
}

export function assertMarketIdentityPhase(value: unknown): asserts value is MarketIdentityPhase {
  const issues = validate("MarketIdentityPhase", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentityPhase", issues);
}

export function parseMarketIdentityPhase(value: unknown): MarketIdentityPhase {
  assertMarketIdentityPhase(value);
  return value;
}

export function validateMarketIdentityPendingAction(value: unknown): ApiIssue[] {
  return validate("MarketIdentityPendingAction", value);
}

export function isMarketIdentityPendingAction(value: unknown): value is MarketIdentityPendingAction {
  return validate("MarketIdentityPendingAction", value).length === 0;
}

export function assertMarketIdentityPendingAction(value: unknown): asserts value is MarketIdentityPendingAction {
  const issues = validate("MarketIdentityPendingAction", value);
  if (issues.length > 0) throw new ApiValidationError("MarketIdentityPendingAction", issues);
}

export function parseMarketIdentityPendingAction(value: unknown): MarketIdentityPendingAction {
  assertMarketIdentityPendingAction(value);
  return value;
}

export function validatePendingActionKind(value: unknown): ApiIssue[] {
  return validate("PendingActionKind", value);
}

export function isPendingActionKind(value: unknown): value is PendingActionKind {
  return validate("PendingActionKind", value).length === 0;
}

export function assertPendingActionKind(value: unknown): asserts value is PendingActionKind {
  const issues = validate("PendingActionKind", value);
  if (issues.length > 0) throw new ApiValidationError("PendingActionKind", issues);
}

export function parsePendingActionKind(value: unknown): PendingActionKind {
  assertPendingActionKind(value);
  return value;
}

export function validateAtprotoRecord(value: unknown): ApiIssue[] {
  return validate("AtprotoRecord", value);
}

export function isAtprotoRecord(value: unknown): value is AtprotoRecord {
  return validate("AtprotoRecord", value).length === 0;
}

export function assertAtprotoRecord(value: unknown): asserts value is AtprotoRecord {
  const issues = validate("AtprotoRecord", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecord", issues);
}

export function parseAtprotoRecord(value: unknown): AtprotoRecord {
  assertAtprotoRecord(value);
  return value;
}

export function validateAtprotoRecordSpec(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordSpec", value);
}

export function isAtprotoRecordSpec(value: unknown): value is AtprotoRecordSpec {
  return validate("AtprotoRecordSpec", value).length === 0;
}

export function assertAtprotoRecordSpec(value: unknown): asserts value is AtprotoRecordSpec {
  const issues = validate("AtprotoRecordSpec", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordSpec", issues);
}

export function parseAtprotoRecordSpec(value: unknown): AtprotoRecordSpec {
  assertAtprotoRecordSpec(value);
  return value;
}

export function validateMissingReferencePolicy(value: unknown): ApiIssue[] {
  return validate("MissingReferencePolicy", value);
}

export function isMissingReferencePolicy(value: unknown): value is MissingReferencePolicy {
  return validate("MissingReferencePolicy", value).length === 0;
}

export function assertMissingReferencePolicy(value: unknown): asserts value is MissingReferencePolicy {
  const issues = validate("MissingReferencePolicy", value);
  if (issues.length > 0) throw new ApiValidationError("MissingReferencePolicy", issues);
}

export function parseMissingReferencePolicy(value: unknown): MissingReferencePolicy {
  assertMissingReferencePolicy(value);
  return value;
}

export function validateAtprotoRecordStatus(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordStatus", value);
}

export function isAtprotoRecordStatus(value: unknown): value is AtprotoRecordStatus {
  return validate("AtprotoRecordStatus", value).length === 0;
}

export function assertAtprotoRecordStatus(value: unknown): asserts value is AtprotoRecordStatus {
  const issues = validate("AtprotoRecordStatus", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordStatus", issues);
}

export function parseAtprotoRecordStatus(value: unknown): AtprotoRecordStatus {
  assertAtprotoRecordStatus(value);
  return value;
}

export function validateAtprotoRecordPhase(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordPhase", value);
}

export function isAtprotoRecordPhase(value: unknown): value is AtprotoRecordPhase {
  return validate("AtprotoRecordPhase", value).length === 0;
}

export function assertAtprotoRecordPhase(value: unknown): asserts value is AtprotoRecordPhase {
  const issues = validate("AtprotoRecordPhase", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordPhase", issues);
}

export function parseAtprotoRecordPhase(value: unknown): AtprotoRecordPhase {
  assertAtprotoRecordPhase(value);
  return value;
}

export function validateAtprotoRecordWatch(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordWatch", value);
}

export function isAtprotoRecordWatch(value: unknown): value is AtprotoRecordWatch {
  return validate("AtprotoRecordWatch", value).length === 0;
}

export function assertAtprotoRecordWatch(value: unknown): asserts value is AtprotoRecordWatch {
  const issues = validate("AtprotoRecordWatch", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordWatch", issues);
}

export function parseAtprotoRecordWatch(value: unknown): AtprotoRecordWatch {
  assertAtprotoRecordWatch(value);
  return value;
}

export function validateAtprotoRecordWatchSpec(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordWatchSpec", value);
}

export function isAtprotoRecordWatchSpec(value: unknown): value is AtprotoRecordWatchSpec {
  return validate("AtprotoRecordWatchSpec", value).length === 0;
}

export function assertAtprotoRecordWatchSpec(value: unknown): asserts value is AtprotoRecordWatchSpec {
  const issues = validate("AtprotoRecordWatchSpec", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordWatchSpec", issues);
}

export function parseAtprotoRecordWatchSpec(value: unknown): AtprotoRecordWatchSpec {
  assertAtprotoRecordWatchSpec(value);
  return value;
}

export function validateAtprotoRecordMatch(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordMatch", value);
}

export function isAtprotoRecordMatch(value: unknown): value is AtprotoRecordMatch {
  return validate("AtprotoRecordMatch", value).length === 0;
}

export function assertAtprotoRecordMatch(value: unknown): asserts value is AtprotoRecordMatch {
  const issues = validate("AtprotoRecordMatch", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordMatch", issues);
}

export function parseAtprotoRecordMatch(value: unknown): AtprotoRecordMatch {
  assertAtprotoRecordMatch(value);
  return value;
}

export function validateAtprotoValueFrom(value: unknown): ApiIssue[] {
  return validate("AtprotoValueFrom", value);
}

export function isAtprotoValueFrom(value: unknown): value is AtprotoValueFrom {
  return validate("AtprotoValueFrom", value).length === 0;
}

export function assertAtprotoValueFrom(value: unknown): asserts value is AtprotoValueFrom {
  const issues = validate("AtprotoValueFrom", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoValueFrom", issues);
}

export function parseAtprotoValueFrom(value: unknown): AtprotoValueFrom {
  assertAtprotoValueFrom(value);
  return value;
}

export function validateAtprotoRecordWatchSource(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordWatchSource", value);
}

export function isAtprotoRecordWatchSource(value: unknown): value is AtprotoRecordWatchSource {
  return validate("AtprotoRecordWatchSource", value).length === 0;
}

export function assertAtprotoRecordWatchSource(value: unknown): asserts value is AtprotoRecordWatchSource {
  const issues = validate("AtprotoRecordWatchSource", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordWatchSource", issues);
}

export function parseAtprotoRecordWatchSource(value: unknown): AtprotoRecordWatchSource {
  assertAtprotoRecordWatchSource(value);
  return value;
}

export function validateAtprotoRecordWatchStatus(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordWatchStatus", value);
}

export function isAtprotoRecordWatchStatus(value: unknown): value is AtprotoRecordWatchStatus {
  return validate("AtprotoRecordWatchStatus", value).length === 0;
}

export function assertAtprotoRecordWatchStatus(value: unknown): asserts value is AtprotoRecordWatchStatus {
  const issues = validate("AtprotoRecordWatchStatus", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordWatchStatus", issues);
}

export function parseAtprotoRecordWatchStatus(value: unknown): AtprotoRecordWatchStatus {
  assertAtprotoRecordWatchStatus(value);
  return value;
}

export function validateWatchedRecord(value: unknown): ApiIssue[] {
  return validate("WatchedRecord", value);
}

export function isWatchedRecord(value: unknown): value is WatchedRecord {
  return validate("WatchedRecord", value).length === 0;
}

export function assertWatchedRecord(value: unknown): asserts value is WatchedRecord {
  const issues = validate("WatchedRecord", value);
  if (issues.length > 0) throw new ApiValidationError("WatchedRecord", issues);
}

export function parseWatchedRecord(value: unknown): WatchedRecord {
  assertWatchedRecord(value);
  return value;
}

export function validateAtprotoRecordWatchCursor(value: unknown): ApiIssue[] {
  return validate("AtprotoRecordWatchCursor", value);
}

export function isAtprotoRecordWatchCursor(value: unknown): value is AtprotoRecordWatchCursor {
  return validate("AtprotoRecordWatchCursor", value).length === 0;
}

export function assertAtprotoRecordWatchCursor(value: unknown): asserts value is AtprotoRecordWatchCursor {
  const issues = validate("AtprotoRecordWatchCursor", value);
  if (issues.length > 0) throw new ApiValidationError("AtprotoRecordWatchCursor", issues);
}

export function parseAtprotoRecordWatchCursor(value: unknown): AtprotoRecordWatchCursor {
  assertAtprotoRecordWatchCursor(value);
  return value;
}

export function validateMarketNetwork(value: unknown): ApiIssue[] {
  return validate("MarketNetwork", value);
}

export function isMarketNetwork(value: unknown): value is MarketNetwork {
  return validate("MarketNetwork", value).length === 0;
}

export function assertMarketNetwork(value: unknown): asserts value is MarketNetwork {
  const issues = validate("MarketNetwork", value);
  if (issues.length > 0) throw new ApiValidationError("MarketNetwork", issues);
}

export function parseMarketNetwork(value: unknown): MarketNetwork {
  assertMarketNetwork(value);
  return value;
}

export function validateMarketNetworkSpec(value: unknown): ApiIssue[] {
  return validate("MarketNetworkSpec", value);
}

export function isMarketNetworkSpec(value: unknown): value is MarketNetworkSpec {
  return validate("MarketNetworkSpec", value).length === 0;
}

export function assertMarketNetworkSpec(value: unknown): asserts value is MarketNetworkSpec {
  const issues = validate("MarketNetworkSpec", value);
  if (issues.length > 0) throw new ApiValidationError("MarketNetworkSpec", issues);
}

export function parseMarketNetworkSpec(value: unknown): MarketNetworkSpec {
  assertMarketNetworkSpec(value);
  return value;
}

export function validateNetworkApp(value: unknown): ApiIssue[] {
  return validate("NetworkApp", value);
}

export function isNetworkApp(value: unknown): value is NetworkApp {
  return validate("NetworkApp", value).length === 0;
}

export function assertNetworkApp(value: unknown): asserts value is NetworkApp {
  const issues = validate("NetworkApp", value);
  if (issues.length > 0) throw new ApiValidationError("NetworkApp", issues);
}

export function parseNetworkApp(value: unknown): NetworkApp {
  assertNetworkApp(value);
  return value;
}

export function validateApplicationKind(value: unknown): ApiIssue[] {
  return validate("ApplicationKind", value);
}

export function isApplicationKind(value: unknown): value is ApplicationKind {
  return validate("ApplicationKind", value).length === 0;
}

export function assertApplicationKind(value: unknown): asserts value is ApplicationKind {
  const issues = validate("ApplicationKind", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationKind", issues);
}

export function parseApplicationKind(value: unknown): ApplicationKind {
  assertApplicationKind(value);
  return value;
}

export function validateFirecrackerProvider(value: unknown): ApiIssue[] {
  return validate("FirecrackerProvider", value);
}

export function isFirecrackerProvider(value: unknown): value is FirecrackerProvider {
  return validate("FirecrackerProvider", value).length === 0;
}

export function assertFirecrackerProvider(value: unknown): asserts value is FirecrackerProvider {
  const issues = validate("FirecrackerProvider", value);
  if (issues.length > 0) throw new ApiValidationError("FirecrackerProvider", issues);
}

export function parseFirecrackerProvider(value: unknown): FirecrackerProvider {
  assertFirecrackerProvider(value);
  return value;
}

export function validateKcpProvider(value: unknown): ApiIssue[] {
  return validate("KcpProvider", value);
}

export function isKcpProvider(value: unknown): value is KcpProvider {
  return validate("KcpProvider", value).length === 0;
}

export function assertKcpProvider(value: unknown): asserts value is KcpProvider {
  const issues = validate("KcpProvider", value);
  if (issues.length > 0) throw new ApiValidationError("KcpProvider", issues);
}

export function parseKcpProvider(value: unknown): KcpProvider {
  assertKcpProvider(value);
  return value;
}

export function validateApplicationOption(value: unknown): ApiIssue[] {
  return validate("ApplicationOption", value);
}

export function isApplicationOption(value: unknown): value is ApplicationOption {
  return validate("ApplicationOption", value).length === 0;
}

export function assertApplicationOption(value: unknown): asserts value is ApplicationOption {
  const issues = validate("ApplicationOption", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationOption", issues);
}

export function parseApplicationOption(value: unknown): ApplicationOption {
  assertApplicationOption(value);
  return value;
}

export function validateApplicationQuota(value: unknown): ApiIssue[] {
  return validate("ApplicationQuota", value);
}

export function isApplicationQuota(value: unknown): value is ApplicationQuota {
  return validate("ApplicationQuota", value).length === 0;
}

export function assertApplicationQuota(value: unknown): asserts value is ApplicationQuota {
  const issues = validate("ApplicationQuota", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationQuota", issues);
}

export function parseApplicationQuota(value: unknown): ApplicationQuota {
  assertApplicationQuota(value);
  return value;
}

export function validateGatewayClusterProvider(value: unknown): ApiIssue[] {
  return validate("GatewayClusterProvider", value);
}

export function isGatewayClusterProvider(value: unknown): value is GatewayClusterProvider {
  return validate("GatewayClusterProvider", value).length === 0;
}

export function assertGatewayClusterProvider(value: unknown): asserts value is GatewayClusterProvider {
  const issues = validate("GatewayClusterProvider", value);
  if (issues.length > 0) throw new ApiValidationError("GatewayClusterProvider", issues);
}

export function parseGatewayClusterProvider(value: unknown): GatewayClusterProvider {
  assertGatewayClusterProvider(value);
  return value;
}

export function validateMarketNetworkStatus(value: unknown): ApiIssue[] {
  return validate("MarketNetworkStatus", value);
}

export function isMarketNetworkStatus(value: unknown): value is MarketNetworkStatus {
  return validate("MarketNetworkStatus", value).length === 0;
}

export function assertMarketNetworkStatus(value: unknown): asserts value is MarketNetworkStatus {
  const issues = validate("MarketNetworkStatus", value);
  if (issues.length > 0) throw new ApiValidationError("MarketNetworkStatus", issues);
}

export function parseMarketNetworkStatus(value: unknown): MarketNetworkStatus {
  assertMarketNetworkStatus(value);
  return value;
}

export function validateApplicationPhase(value: unknown): ApiIssue[] {
  return validate("ApplicationPhase", value);
}

export function isApplicationPhase(value: unknown): value is ApplicationPhase {
  return validate("ApplicationPhase", value).length === 0;
}

export function assertApplicationPhase(value: unknown): asserts value is ApplicationPhase {
  const issues = validate("ApplicationPhase", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationPhase", issues);
}

export function parseApplicationPhase(value: unknown): ApplicationPhase {
  assertApplicationPhase(value);
  return value;
}

export function validateMarketNetworkValue(value: unknown): ApiIssue[] {
  return validate("MarketNetworkValue", value);
}

export function isMarketNetworkValue(value: unknown): value is MarketNetworkValue {
  return validate("MarketNetworkValue", value).length === 0;
}

export function assertMarketNetworkValue(value: unknown): asserts value is MarketNetworkValue {
  const issues = validate("MarketNetworkValue", value);
  if (issues.length > 0) throw new ApiValidationError("MarketNetworkValue", issues);
}

export function parseMarketNetworkValue(value: unknown): MarketNetworkValue {
  assertMarketNetworkValue(value);
  return value;
}

export function validateNetworkHost(value: unknown): ApiIssue[] {
  return validate("NetworkHost", value);
}

export function isNetworkHost(value: unknown): value is NetworkHost {
  return validate("NetworkHost", value).length === 0;
}

export function assertNetworkHost(value: unknown): asserts value is NetworkHost {
  const issues = validate("NetworkHost", value);
  if (issues.length > 0) throw new ApiValidationError("NetworkHost", issues);
}

export function parseNetworkHost(value: unknown): NetworkHost {
  assertNetworkHost(value);
  return value;
}

export function validateApplicationComponent(value: unknown): ApiIssue[] {
  return validate("ApplicationComponent", value);
}

export function isApplicationComponent(value: unknown): value is ApplicationComponent {
  return validate("ApplicationComponent", value).length === 0;
}

export function assertApplicationComponent(value: unknown): asserts value is ApplicationComponent {
  const issues = validate("ApplicationComponent", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationComponent", issues);
}

export function parseApplicationComponent(value: unknown): ApplicationComponent {
  assertApplicationComponent(value);
  return value;
}

export function validateApplicationListener(value: unknown): ApiIssue[] {
  return validate("ApplicationListener", value);
}

export function isApplicationListener(value: unknown): value is ApplicationListener {
  return validate("ApplicationListener", value).length === 0;
}

export function assertApplicationListener(value: unknown): asserts value is ApplicationListener {
  const issues = validate("ApplicationListener", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationListener", issues);
}

export function parseApplicationListener(value: unknown): ApplicationListener {
  assertApplicationListener(value);
  return value;
}

export function validateNetworkAppStatus(value: unknown): ApiIssue[] {
  return validate("NetworkAppStatus", value);
}

export function isNetworkAppStatus(value: unknown): value is NetworkAppStatus {
  return validate("NetworkAppStatus", value).length === 0;
}

export function assertNetworkAppStatus(value: unknown): asserts value is NetworkAppStatus {
  const issues = validate("NetworkAppStatus", value);
  if (issues.length > 0) throw new ApiValidationError("NetworkAppStatus", issues);
}

export function parseNetworkAppStatus(value: unknown): NetworkAppStatus {
  assertNetworkAppStatus(value);
  return value;
}

export function validateStateDirUsage(value: unknown): ApiIssue[] {
  return validate("StateDirUsage", value);
}

export function isStateDirUsage(value: unknown): value is StateDirUsage {
  return validate("StateDirUsage", value).length === 0;
}

export function assertStateDirUsage(value: unknown): asserts value is StateDirUsage {
  const issues = validate("StateDirUsage", value);
  if (issues.length > 0) throw new ApiValidationError("StateDirUsage", issues);
}

export function parseStateDirUsage(value: unknown): StateDirUsage {
  assertStateDirUsage(value);
  return value;
}

export function validateGCStatus(value: unknown): ApiIssue[] {
  return validate("GCStatus", value);
}

export function isGCStatus(value: unknown): value is GCStatus {
  return validate("GCStatus", value).length === 0;
}

export function assertGCStatus(value: unknown): asserts value is GCStatus {
  const issues = validate("GCStatus", value);
  if (issues.length > 0) throw new ApiValidationError("GCStatus", issues);
}

export function parseGCStatus(value: unknown): GCStatus {
  assertGCStatus(value);
  return value;
}

export function validateGCKindCount(value: unknown): ApiIssue[] {
  return validate("GCKindCount", value);
}

export function isGCKindCount(value: unknown): value is GCKindCount {
  return validate("GCKindCount", value).length === 0;
}

export function assertGCKindCount(value: unknown): asserts value is GCKindCount {
  const issues = validate("GCKindCount", value);
  if (issues.length > 0) throw new ApiValidationError("GCKindCount", issues);
}

export function parseGCKindCount(value: unknown): GCKindCount {
  assertGCKindCount(value);
  return value;
}

export function validateGCStuck(value: unknown): ApiIssue[] {
  return validate("GCStuck", value);
}

export function isGCStuck(value: unknown): value is GCStuck {
  return validate("GCStuck", value).length === 0;
}

export function assertGCStuck(value: unknown): asserts value is GCStuck {
  const issues = validate("GCStuck", value);
  if (issues.length > 0) throw new ApiValidationError("GCStuck", issues);
}

export function parseGCStuck(value: unknown): GCStuck {
  assertGCStuck(value);
  return value;
}

export function validateTTLStatus(value: unknown): ApiIssue[] {
  return validate("TTLStatus", value);
}

export function isTTLStatus(value: unknown): value is TTLStatus {
  return validate("TTLStatus", value).length === 0;
}

export function assertTTLStatus(value: unknown): asserts value is TTLStatus {
  const issues = validate("TTLStatus", value);
  if (issues.length > 0) throw new ApiValidationError("TTLStatus", issues);
}

export function parseTTLStatus(value: unknown): TTLStatus {
  assertTTLStatus(value);
  return value;
}

export function validateTTLDeleted(value: unknown): ApiIssue[] {
  return validate("TTLDeleted", value);
}

export function isTTLDeleted(value: unknown): value is TTLDeleted {
  return validate("TTLDeleted", value).length === 0;
}

export function assertTTLDeleted(value: unknown): asserts value is TTLDeleted {
  const issues = validate("TTLDeleted", value);
  if (issues.length > 0) throw new ApiValidationError("TTLDeleted", issues);
}

export function parseTTLDeleted(value: unknown): TTLDeleted {
  assertTTLDeleted(value);
  return value;
}

export function validateMarketBidder(value: unknown): ApiIssue[] {
  return validate("MarketBidder", value);
}

export function isMarketBidder(value: unknown): value is MarketBidder {
  return validate("MarketBidder", value).length === 0;
}

export function assertMarketBidder(value: unknown): asserts value is MarketBidder {
  const issues = validate("MarketBidder", value);
  if (issues.length > 0) throw new ApiValidationError("MarketBidder", issues);
}

export function parseMarketBidder(value: unknown): MarketBidder {
  assertMarketBidder(value);
  return value;
}

export function validateMarketBidderSpec(value: unknown): ApiIssue[] {
  return validate("MarketBidderSpec", value);
}

export function isMarketBidderSpec(value: unknown): value is MarketBidderSpec {
  return validate("MarketBidderSpec", value).length === 0;
}

export function assertMarketBidderSpec(value: unknown): asserts value is MarketBidderSpec {
  const issues = validate("MarketBidderSpec", value);
  if (issues.length > 0) throw new ApiValidationError("MarketBidderSpec", issues);
}

export function parseMarketBidderSpec(value: unknown): MarketBidderSpec {
  assertMarketBidderSpec(value);
  return value;
}

export function validateApplicationIdentity(value: unknown): ApiIssue[] {
  return validate("ApplicationIdentity", value);
}

export function isApplicationIdentity(value: unknown): value is ApplicationIdentity {
  return validate("ApplicationIdentity", value).length === 0;
}

export function assertApplicationIdentity(value: unknown): asserts value is ApplicationIdentity {
  const issues = validate("ApplicationIdentity", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationIdentity", issues);
}

export function parseApplicationIdentity(value: unknown): ApplicationIdentity {
  assertApplicationIdentity(value);
  return value;
}

export function validateApplicationStatus(value: unknown): ApiIssue[] {
  return validate("ApplicationStatus", value);
}

export function isApplicationStatus(value: unknown): value is ApplicationStatus {
  return validate("ApplicationStatus", value).length === 0;
}

export function assertApplicationStatus(value: unknown): asserts value is ApplicationStatus {
  const issues = validate("ApplicationStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ApplicationStatus", issues);
}

export function parseApplicationStatus(value: unknown): ApplicationStatus {
  assertApplicationStatus(value);
  return value;
}

export function validateStoreUsage(value: unknown): ApiIssue[] {
  return validate("StoreUsage", value);
}

export function isStoreUsage(value: unknown): value is StoreUsage {
  return validate("StoreUsage", value).length === 0;
}

export function assertStoreUsage(value: unknown): asserts value is StoreUsage {
  const issues = validate("StoreUsage", value);
  if (issues.length > 0) throw new ApiValidationError("StoreUsage", issues);
}

export function parseStoreUsage(value: unknown): StoreUsage {
  assertStoreUsage(value);
  return value;
}

export function validateQuotaUsage(value: unknown): ApiIssue[] {
  return validate("QuotaUsage", value);
}

export function isQuotaUsage(value: unknown): value is QuotaUsage {
  return validate("QuotaUsage", value).length === 0;
}

export function assertQuotaUsage(value: unknown): asserts value is QuotaUsage {
  const issues = validate("QuotaUsage", value);
  if (issues.length > 0) throw new ApiValidationError("QuotaUsage", issues);
}

export function parseQuotaUsage(value: unknown): QuotaUsage {
  assertQuotaUsage(value);
  return value;
}

export function validateMarketLoopStatus(value: unknown): ApiIssue[] {
  return validate("MarketLoopStatus", value);
}

export function isMarketLoopStatus(value: unknown): value is MarketLoopStatus {
  return validate("MarketLoopStatus", value).length === 0;
}

export function assertMarketLoopStatus(value: unknown): asserts value is MarketLoopStatus {
  const issues = validate("MarketLoopStatus", value);
  if (issues.length > 0) throw new ApiValidationError("MarketLoopStatus", issues);
}

export function parseMarketLoopStatus(value: unknown): MarketLoopStatus {
  assertMarketLoopStatus(value);
  return value;
}

export function validateMarketDeclineCount(value: unknown): ApiIssue[] {
  return validate("MarketDeclineCount", value);
}

export function isMarketDeclineCount(value: unknown): value is MarketDeclineCount {
  return validate("MarketDeclineCount", value).length === 0;
}

export function assertMarketDeclineCount(value: unknown): asserts value is MarketDeclineCount {
  const issues = validate("MarketDeclineCount", value);
  if (issues.length > 0) throw new ApiValidationError("MarketDeclineCount", issues);
}

export function parseMarketDeclineCount(value: unknown): MarketDeclineCount {
  assertMarketDeclineCount(value);
  return value;
}

export function validateMarketStreamStatus(value: unknown): ApiIssue[] {
  return validate("MarketStreamStatus", value);
}

export function isMarketStreamStatus(value: unknown): value is MarketStreamStatus {
  return validate("MarketStreamStatus", value).length === 0;
}

export function assertMarketStreamStatus(value: unknown): asserts value is MarketStreamStatus {
  const issues = validate("MarketStreamStatus", value);
  if (issues.length > 0) throw new ApiValidationError("MarketStreamStatus", issues);
}

export function parseMarketStreamStatus(value: unknown): MarketStreamStatus {
  assertMarketStreamStatus(value);
  return value;
}

export function validateMarketDecline(value: unknown): ApiIssue[] {
  return validate("MarketDecline", value);
}

export function isMarketDecline(value: unknown): value is MarketDecline {
  return validate("MarketDecline", value).length === 0;
}

export function assertMarketDecline(value: unknown): asserts value is MarketDecline {
  const issues = validate("MarketDecline", value);
  if (issues.length > 0) throw new ApiValidationError("MarketDecline", issues);
}

export function parseMarketDecline(value: unknown): MarketDecline {
  assertMarketDecline(value);
  return value;
}

export function validateMarketLatencyStatus(value: unknown): ApiIssue[] {
  return validate("MarketLatencyStatus", value);
}

export function isMarketLatencyStatus(value: unknown): value is MarketLatencyStatus {
  return validate("MarketLatencyStatus", value).length === 0;
}

export function assertMarketLatencyStatus(value: unknown): asserts value is MarketLatencyStatus {
  const issues = validate("MarketLatencyStatus", value);
  if (issues.length > 0) throw new ApiValidationError("MarketLatencyStatus", issues);
}

export function parseMarketLatencyStatus(value: unknown): MarketLatencyStatus {
  assertMarketLatencyStatus(value);
  return value;
}

export function validatePdsWriteStatus(value: unknown): ApiIssue[] {
  return validate("PdsWriteStatus", value);
}

export function isPdsWriteStatus(value: unknown): value is PdsWriteStatus {
  return validate("PdsWriteStatus", value).length === 0;
}

export function assertPdsWriteStatus(value: unknown): asserts value is PdsWriteStatus {
  const issues = validate("PdsWriteStatus", value);
  if (issues.length > 0) throw new ApiValidationError("PdsWriteStatus", issues);
}

export function parsePdsWriteStatus(value: unknown): PdsWriteStatus {
  assertPdsWriteStatus(value);
  return value;
}

export function validatePdsWriteCollection(value: unknown): ApiIssue[] {
  return validate("PdsWriteCollection", value);
}

export function isPdsWriteCollection(value: unknown): value is PdsWriteCollection {
  return validate("PdsWriteCollection", value).length === 0;
}

export function assertPdsWriteCollection(value: unknown): asserts value is PdsWriteCollection {
  const issues = validate("PdsWriteCollection", value);
  if (issues.length > 0) throw new ApiValidationError("PdsWriteCollection", issues);
}

export function parsePdsWriteCollection(value: unknown): PdsWriteCollection {
  assertPdsWriteCollection(value);
  return value;
}

export function validatePdsWriteHour(value: unknown): ApiIssue[] {
  return validate("PdsWriteHour", value);
}

export function isPdsWriteHour(value: unknown): value is PdsWriteHour {
  return validate("PdsWriteHour", value).length === 0;
}

export function assertPdsWriteHour(value: unknown): asserts value is PdsWriteHour {
  const issues = validate("PdsWriteHour", value);
  if (issues.length > 0) throw new ApiValidationError("PdsWriteHour", issues);
}

export function parsePdsWriteHour(value: unknown): PdsWriteHour {
  assertPdsWriteHour(value);
  return value;
}

export function validatePdsLimitStatus(value: unknown): ApiIssue[] {
  return validate("PdsLimitStatus", value);
}

export function isPdsLimitStatus(value: unknown): value is PdsLimitStatus {
  return validate("PdsLimitStatus", value).length === 0;
}

export function assertPdsLimitStatus(value: unknown): asserts value is PdsLimitStatus {
  const issues = validate("PdsLimitStatus", value);
  if (issues.length > 0) throw new ApiValidationError("PdsLimitStatus", issues);
}

export function parsePdsLimitStatus(value: unknown): PdsLimitStatus {
  assertPdsLimitStatus(value);
  return value;
}

export function validateMarketRequester(value: unknown): ApiIssue[] {
  return validate("MarketRequester", value);
}

export function isMarketRequester(value: unknown): value is MarketRequester {
  return validate("MarketRequester", value).length === 0;
}

export function assertMarketRequester(value: unknown): asserts value is MarketRequester {
  const issues = validate("MarketRequester", value);
  if (issues.length > 0) throw new ApiValidationError("MarketRequester", issues);
}

export function parseMarketRequester(value: unknown): MarketRequester {
  assertMarketRequester(value);
  return value;
}

export function validateMarketRequesterSpec(value: unknown): ApiIssue[] {
  return validate("MarketRequesterSpec", value);
}

export function isMarketRequesterSpec(value: unknown): value is MarketRequesterSpec {
  return validate("MarketRequesterSpec", value).length === 0;
}

export function assertMarketRequesterSpec(value: unknown): asserts value is MarketRequesterSpec {
  const issues = validate("MarketRequesterSpec", value);
  if (issues.length > 0) throw new ApiValidationError("MarketRequesterSpec", issues);
}

export function parseMarketRequesterSpec(value: unknown): MarketRequesterSpec {
  assertMarketRequesterSpec(value);
  return value;
}

export function validateComputeContract(value: unknown): ApiIssue[] {
  return validate("ComputeContract", value);
}

export function isComputeContract(value: unknown): value is ComputeContract {
  return validate("ComputeContract", value).length === 0;
}

export function assertComputeContract(value: unknown): asserts value is ComputeContract {
  const issues = validate("ComputeContract", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContract", issues);
}

export function parseComputeContract(value: unknown): ComputeContract {
  assertComputeContract(value);
  return value;
}

export function validateComputeContractSpec(value: unknown): ApiIssue[] {
  return validate("ComputeContractSpec", value);
}

export function isComputeContractSpec(value: unknown): value is ComputeContractSpec {
  return validate("ComputeContractSpec", value).length === 0;
}

export function assertComputeContractSpec(value: unknown): asserts value is ComputeContractSpec {
  const issues = validate("ComputeContractSpec", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractSpec", issues);
}

export function parseComputeContractSpec(value: unknown): ComputeContractSpec {
  assertComputeContractSpec(value);
  return value;
}

export function validateComputeContractNodeJoin(value: unknown): ApiIssue[] {
  return validate("ComputeContractNodeJoin", value);
}

export function isComputeContractNodeJoin(value: unknown): value is ComputeContractNodeJoin {
  return validate("ComputeContractNodeJoin", value).length === 0;
}

export function assertComputeContractNodeJoin(value: unknown): asserts value is ComputeContractNodeJoin {
  const issues = validate("ComputeContractNodeJoin", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractNodeJoin", issues);
}

export function parseComputeContractNodeJoin(value: unknown): ComputeContractNodeJoin {
  assertComputeContractNodeJoin(value);
  return value;
}

export function validateComputeContractMeshd(value: unknown): ApiIssue[] {
  return validate("ComputeContractMeshd", value);
}

export function isComputeContractMeshd(value: unknown): value is ComputeContractMeshd {
  return validate("ComputeContractMeshd", value).length === 0;
}

export function assertComputeContractMeshd(value: unknown): asserts value is ComputeContractMeshd {
  const issues = validate("ComputeContractMeshd", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractMeshd", issues);
}

export function parseComputeContractMeshd(value: unknown): ComputeContractMeshd {
  assertComputeContractMeshd(value);
  return value;
}

export function validateComputeContractMeshdPin(value: unknown): ApiIssue[] {
  return validate("ComputeContractMeshdPin", value);
}

export function isComputeContractMeshdPin(value: unknown): value is ComputeContractMeshdPin {
  return validate("ComputeContractMeshdPin", value).length === 0;
}

export function assertComputeContractMeshdPin(value: unknown): asserts value is ComputeContractMeshdPin {
  const issues = validate("ComputeContractMeshdPin", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractMeshdPin", issues);
}

export function parseComputeContractMeshdPin(value: unknown): ComputeContractMeshdPin {
  assertComputeContractMeshdPin(value);
  return value;
}

export function validateComputeContractStatus(value: unknown): ApiIssue[] {
  return validate("ComputeContractStatus", value);
}

export function isComputeContractStatus(value: unknown): value is ComputeContractStatus {
  return validate("ComputeContractStatus", value).length === 0;
}

export function assertComputeContractStatus(value: unknown): asserts value is ComputeContractStatus {
  const issues = validate("ComputeContractStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractStatus", issues);
}

export function parseComputeContractStatus(value: unknown): ComputeContractStatus {
  assertComputeContractStatus(value);
  return value;
}

export function validateComputeContractPhase(value: unknown): ApiIssue[] {
  return validate("ComputeContractPhase", value);
}

export function isComputeContractPhase(value: unknown): value is ComputeContractPhase {
  return validate("ComputeContractPhase", value).length === 0;
}

export function assertComputeContractPhase(value: unknown): asserts value is ComputeContractPhase {
  const issues = validate("ComputeContractPhase", value);
  if (issues.length > 0) throw new ApiValidationError("ComputeContractPhase", issues);
}

export function parseComputeContractPhase(value: unknown): ComputeContractPhase {
  assertComputeContractPhase(value);
  return value;
}

export function validateContractBid(value: unknown): ApiIssue[] {
  return validate("ContractBid", value);
}

export function isContractBid(value: unknown): value is ContractBid {
  return validate("ContractBid", value).length === 0;
}

export function assertContractBid(value: unknown): asserts value is ContractBid {
  const issues = validate("ContractBid", value);
  if (issues.length > 0) throw new ApiValidationError("ContractBid", issues);
}

export function parseContractBid(value: unknown): ContractBid {
  assertContractBid(value);
  return value;
}

export function validateContractBidVerdict(value: unknown): ApiIssue[] {
  return validate("ContractBidVerdict", value);
}

export function isContractBidVerdict(value: unknown): value is ContractBidVerdict {
  return validate("ContractBidVerdict", value).length === 0;
}

export function assertContractBidVerdict(value: unknown): asserts value is ContractBidVerdict {
  const issues = validate("ContractBidVerdict", value);
  if (issues.length > 0) throw new ApiValidationError("ContractBidVerdict", issues);
}

export function parseContractBidVerdict(value: unknown): ContractBidVerdict {
  assertContractBidVerdict(value);
  return value;
}

export function validateContractChosen(value: unknown): ApiIssue[] {
  return validate("ContractChosen", value);
}

export function isContractChosen(value: unknown): value is ContractChosen {
  return validate("ContractChosen", value).length === 0;
}

export function assertContractChosen(value: unknown): asserts value is ContractChosen {
  const issues = validate("ContractChosen", value);
  if (issues.length > 0) throw new ApiValidationError("ContractChosen", issues);
}

export function parseContractChosen(value: unknown): ContractChosen {
  assertContractChosen(value);
  return value;
}

export function validateContractChosenReason(value: unknown): ApiIssue[] {
  return validate("ContractChosenReason", value);
}

export function isContractChosenReason(value: unknown): value is ContractChosenReason {
  return validate("ContractChosenReason", value).length === 0;
}

export function assertContractChosenReason(value: unknown): asserts value is ContractChosenReason {
  const issues = validate("ContractChosenReason", value);
  if (issues.length > 0) throw new ApiValidationError("ContractChosenReason", issues);
}

export function parseContractChosenReason(value: unknown): ContractChosenReason {
  assertContractChosenReason(value);
  return value;
}

export function validateExecutionStatus(value: unknown): ApiIssue[] {
  return validate("ExecutionStatus", value);
}

export function isExecutionStatus(value: unknown): value is ExecutionStatus {
  return validate("ExecutionStatus", value).length === 0;
}

export function assertExecutionStatus(value: unknown): asserts value is ExecutionStatus {
  const issues = validate("ExecutionStatus", value);
  if (issues.length > 0) throw new ApiValidationError("ExecutionStatus", issues);
}

export function parseExecutionStatus(value: unknown): ExecutionStatus {
  assertExecutionStatus(value);
  return value;
}

export function validateGateway(value: unknown): ApiIssue[] {
  return validate("Gateway", value);
}

export function isGateway(value: unknown): value is Gateway {
  return validate("Gateway", value).length === 0;
}

export function assertGateway(value: unknown): asserts value is Gateway {
  const issues = validate("Gateway", value);
  if (issues.length > 0) throw new ApiValidationError("Gateway", issues);
}

export function parseGateway(value: unknown): Gateway {
  assertGateway(value);
  return value;
}

export function validateGatewaySpec(value: unknown): ApiIssue[] {
  return validate("GatewaySpec", value);
}

export function isGatewaySpec(value: unknown): value is GatewaySpec {
  return validate("GatewaySpec", value).length === 0;
}

export function assertGatewaySpec(value: unknown): asserts value is GatewaySpec {
  const issues = validate("GatewaySpec", value);
  if (issues.length > 0) throw new ApiValidationError("GatewaySpec", issues);
}

export function parseGatewaySpec(value: unknown): GatewaySpec {
  assertGatewaySpec(value);
  return value;
}

export function validateGatewayMarketNetwork(value: unknown): ApiIssue[] {
  return validate("GatewayMarketNetwork", value);
}

export function isGatewayMarketNetwork(value: unknown): value is GatewayMarketNetwork {
  return validate("GatewayMarketNetwork", value).length === 0;
}

export function assertGatewayMarketNetwork(value: unknown): asserts value is GatewayMarketNetwork {
  const issues = validate("GatewayMarketNetwork", value);
  if (issues.length > 0) throw new ApiValidationError("GatewayMarketNetwork", issues);
}

export function parseGatewayMarketNetwork(value: unknown): GatewayMarketNetwork {
  assertGatewayMarketNetwork(value);
  return value;
}

export function validateSshGateway(value: unknown): ApiIssue[] {
  return validate("SshGateway", value);
}

export function isSshGateway(value: unknown): value is SshGateway {
  return validate("SshGateway", value).length === 0;
}

export function assertSshGateway(value: unknown): asserts value is SshGateway {
  const issues = validate("SshGateway", value);
  if (issues.length > 0) throw new ApiValidationError("SshGateway", issues);
}

export function parseSshGateway(value: unknown): SshGateway {
  assertSshGateway(value);
  return value;
}

export function validateSshGatewaySpec(value: unknown): ApiIssue[] {
  return validate("SshGatewaySpec", value);
}

export function isSshGatewaySpec(value: unknown): value is SshGatewaySpec {
  return validate("SshGatewaySpec", value).length === 0;
}

export function assertSshGatewaySpec(value: unknown): asserts value is SshGatewaySpec {
  const issues = validate("SshGatewaySpec", value);
  if (issues.length > 0) throw new ApiValidationError("SshGatewaySpec", issues);
}

export function parseSshGatewaySpec(value: unknown): SshGatewaySpec {
  assertSshGatewaySpec(value);
  return value;
}

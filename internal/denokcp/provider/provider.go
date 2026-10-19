package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/joballoc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/probe"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/runner"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/runref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/factory/admission"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/factory/servicenames"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/metrics"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/openbaoclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/pkiprovisioner"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/denojob"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/denopod"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/denorun"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/policyengine"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/policyworkflowpod"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/policyworkflowrun"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/trigger"
)

type Instances interface {
	Read(ctx context.Context, ref Ref) (*v1alpha1.PolicyWorkflowRun, error)

	WriteStatus(ctx context.Context, ref Ref, st v1alpha1.PolicyWorkflowRunStatus) error

	Delete(ctx context.Context, ref Ref) error

	RemoveFinalizer(ctx context.Context, ref Ref) error
}

var _ Instances = (*Registry)(nil)

type Reader interface {
	Read(ctx context.Context, ref Ref) (*v1alpha1.PolicyWorkflowRun, error)

	ReadRun(ctx context.Context, ref Ref) (*v1alpha1.DenoRun, error)

	ListRuns(ctx context.Context, logicalCluster string) ([]v1alpha1.DenoRun, error)

	ReadPod(ctx context.Context, ref Ref) (*v1alpha1.DenoPod, error)

	ReadJob(ctx context.Context, ref Ref) (*v1alpha1.DenoJob, error)

	ReadTrigger(ctx context.Context, ref Ref) (*v1alpha1.RunTrigger, error)

	ReadEngine(ctx context.Context, ref Ref) (*v1alpha1.PolicyEngine, error)

	ReadOpenBao(ctx context.Context, ref Ref) (*v1alpha1.OpenBao, error)

	ReadWorkflowPod(ctx context.Context, ref Ref) (*v1alpha1.PolicyWorkflowPod, error)

	ListWorkflowRuns(ctx context.Context, logicalCluster string) ([]v1alpha1.PolicyWorkflowRun, error)
}

var _ Reader = (*Registry)(nil)

type Options struct {
	Assets string

	Registry Instances

	Reader Reader

	RestConfig *rest.Config

	ProviderWorkspace string

	PolicyClient policy.Client

	Decider *policyworkflowrun.Reconciler

	Runtime Runtime

	Minter TokenMinter

	ProjectedSources ProjectedSources

	PodRunner runner.PodRunner

	EngineRunner runner.EngineRunner

	PodDecider *denopod.Reconciler

	RunDecider *denorun.Reconciler

	JobDecider *denojob.Reconciler

	TriggerDecider *trigger.Reconciler

	EngineDecider *policyengine.Reconciler

	WorkflowPodDecider *policyworkflowpod.Reconciler

	Host string

	TokenTTL time.Duration

	BundledActionsDir string

	MetricsListen string

	DefaultRunTTLSeconds *int64

	WriteStatus bool

	Interval time.Duration

	MinTransitionPoll time.Duration

	WatchWorkers int

	RunsDir string

	Deno string

	ServiceDomain string

	OpenBaoAddress string

	OpenBaoToken string

	OpenBaoCACert []byte

	OpenBaoClientCert []byte

	OpenBaoClientKey []byte

	OpenBaoMount string

	OpenBaoRole string

	OpenBaoIntermediateTTL string

	OpenBaoLeafTTL string

	Now func() time.Time

	Log *slog.Logger
}

type Provider struct {
	opts Options

	probes *probe.Tracker

	reader Reader

	watch *watchState

	admissions *admission.Admitter

	reconciles atomic.Uint64

	metrics providerMetrics

	metricsSrv *metrics.Server

	services *servicenames.Resolver

	servicesOnce sync.Once

	jobWriteMu sync.Mutex

	jobWriteAt map[string]time.Time

	// ponytail: a created run counts active until the informer observes it, bounded by the allocator TTL so a run deleted before it is seen cannot pin the job forever.
	jobAlloc *joballoc.Allocator

	runRefs *runref.Index

	activeRuns atomic.Int64

	maxActiveRuns atomic.Int64

	runStarts atomic.Int64

	dnsShim string

	dnsProbe string

	dnsTable string

	pki *pkiprovisioner.Provisioner

	clusterCA []byte

	paths *kcpstore.PathCache

	projected projectedStore
}

type Pass struct {
	Phase v1alpha1.PolicyWorkflowPhase

	RequeueAfter time.Duration
}

func New(opts Options) (*Provider, error) {
	if opts.Registry == nil {
		return nil, errors.New("provider: Registry is required")
	}
	if opts.Decider == nil {
		opts.Decider = policyworkflowrun.New(policyworkflowrun.Options{})
	}
	if opts.PodDecider == nil {
		opts.PodDecider = denopod.New(denopod.Options{})
	}
	if opts.RunDecider == nil {
		opts.RunDecider = denorun.New(denorun.Options{})
	}
	if opts.JobDecider == nil {
		opts.JobDecider = denojob.New(denojob.Options{})
	}
	if opts.TriggerDecider == nil {
		opts.TriggerDecider = trigger.New(trigger.Options{})
	}
	if opts.EngineDecider == nil {
		opts.EngineDecider = policyengine.New(policyengine.Options{})
	}
	if opts.WorkflowPodDecider == nil {
		opts.WorkflowPodDecider = policyworkflowpod.New(policyworkflowpod.Options{})
	}
	if opts.TokenTTL == 0 {
		opts.TokenTTL = time.Hour
	}
	if opts.Interval == 0 {
		opts.Interval = defaultRequeueAfter
	}
	if opts.MinTransitionPoll <= 0 {
		opts.MinTransitionPoll = minTransitionPoll
	}
	if opts.WatchWorkers <= 0 {
		opts.WatchWorkers = watchWorkers()
	}
	if opts.RunsDir == "" {
		opts.RunsDir = "runs"
	}
	if opts.ServiceDomain == "" {
		opts.ServiceDomain = kcp.DefaultServiceDomain
	}
	if opts.RestConfig == nil {
		return nil, errors.New("provider: RestConfig is required for the watch driver")
	}
	if opts.ProviderWorkspace == "" {
		opts.ProviderWorkspace = defaultProviderWorkspace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.BundledActionsDir != "" {
		if abs, err := filepath.Abs(opts.BundledActionsDir); err == nil {
			opts.BundledActionsDir = abs
		}
	}
	p := &Provider{opts: opts, probes: probe.NewTracker(), reader: opts.Reader, jobWriteAt: map[string]time.Time{}, jobAlloc: joballoc.New(2 * time.Minute), runRefs: runref.New(runRefTTL), paths: kcpstore.NewPathCache(storeOf(opts.Registry))}
	p.admissions = newRunAdmitter(p)
	p.initMetrics()
	// ponytail: a failure here degrades rather than stops the provider. The shim
	// is how a workload resolves a peer by name; a read-only or missing runs
	// directory should cost that feature, not the whole controller.
	if paths, err := assets.DNSSet(opts.RunsDir).Materialise(); err != nil {
		opts.Log.Warn("kcpdns: the preload shim was not written, workloads will not resolve service names", "err", err)
	} else {
		p.dnsShim = paths[assets.ShimName]
		p.dnsProbe = paths[assets.ProbeName]
		p.dnsTable = assets.DNSTablePath(opts.RunsDir)
	}
	p.services = p.buildServices()
	if opts.RestConfig != nil {
		p.clusterCA = opts.RestConfig.CAData
	}
	// ponytail: same degradation as the shim. Without OpenBao the workloads serve
	// plain HTTP and the FQDN layer still works; an unreachable vault should cost
	// certificates, not the controller. The connection is not tested here because
	// the provider starts before the vault may, and the first workload that asks
	// for a certificate is the first thing that needs it.
	if opts.OpenBaoAddress != "" {
		client, err := openbaoclient.New(openbaoclient.Options{
			Address:    opts.OpenBaoAddress,
			Token:      opts.OpenBaoToken,
			CACert:     opts.OpenBaoCACert,
			ClientCert: opts.OpenBaoClientCert,
			ClientKey:  opts.OpenBaoClientKey,
		})
		if err != nil {
			opts.Log.Warn("openbao: no client, workloads will not serve TLS", "err", err)
		} else {
			pki, err := pkiprovisioner.New(pkiprovisioner.Options{
				Client:          client,
				Mount:           opts.OpenBaoMount,
				Role:            opts.OpenBaoRole,
				IntermediateTTL: opts.OpenBaoIntermediateTTL,
				LeafTTL:         opts.OpenBaoLeafTTL,
				Domain:          opts.ServiceDomain,
			})
			if err != nil {
				opts.Log.Warn("openbao: no provisioner, workloads will not serve TLS", "err", err)
			} else {
				p.pki = pki
			}
		}
	}
	if opts.MetricsListen != "" {
		if err := p.serveMetrics(opts.MetricsListen); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Provider) TrustBundle() []byte {
	bundle := append([]byte(nil), p.clusterCA...)
	if p.pki == nil {
		return bundle
	}
	return append(bundle, p.pki.CachedRootPEM()...)
}

func (p *Provider) Close() error {
	if p.metricsSrv != nil {
		_ = p.metricsSrv.Close()
	}
	return nil
}

func (p *Provider) Run(ctx context.Context) error {
	return p.RunWatch(ctx)
}

func (p *Provider) Reconciles() uint64 {
	return p.reconciles.Load()
}

func (p *Provider) wake(kind workKind, ref Ref) {
	if p.watch == nil {
		return
	}
	p.watch.queue.Add(workKey{kind: kind, ref: Ref{LogicalCluster: ref.LogicalCluster, Namespace: ref.Namespace, Name: ref.Name}})
}

func (p *Provider) readSource() (Reader, error) {
	if p.reader != nil {
		return p.reader, nil
	}
	if r, ok := p.opts.Runtime.(Reader); ok {
		return r, nil
	}
	return nil, errors.New("provider: no reader is configured")
}

func (p *Provider) readPolicyRun(ctx context.Context, ref Ref) (*v1alpha1.PolicyWorkflowRun, error) {
	if p.reader != nil {
		return p.reader.Read(ctx, ref)
	}
	return p.opts.Registry.Read(ctx, ref)
}

func (p *Provider) readRun(ctx context.Context, ref Ref) (*v1alpha1.DenoRun, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadRun(ctx, ref)
}

func (p *Provider) listRuns(ctx context.Context, logicalCluster string) ([]v1alpha1.DenoRun, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ListRuns(ctx, logicalCluster)
}

type jobRunLister interface {
	ListRunsForJob(ctx context.Context, logicalCluster, namespace, jobName string) ([]v1alpha1.DenoRun, error)
}

// ponytail: the watch cache answers a job-scoped list from a label index; a reader without one lists and filters. The lister signature carries the namespace, so a cache whose method omits it stops satisfying this interface and the provider silently falls back to listing the whole workspace; the namespace argument is load-bearing, not decoration.
func (p *Provider) listRunsForJob(ctx context.Context, logicalCluster, namespace, jobName string) ([]v1alpha1.DenoRun, error) {
	if lister, ok := p.reader.(jobRunLister); ok {
		return lister.ListRunsForJob(ctx, logicalCluster, namespace, jobName)
	}
	runs, err := p.listRuns(ctx, logicalCluster)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.DenoRun
	for i := range runs {
		if runs[i].Namespace == namespace && runs[i].Labels[JobRunLabel] == jobName {
			out = append(out, runs[i])
		}
	}
	return out, nil
}

func (p *Provider) readPod(ctx context.Context, ref Ref) (*v1alpha1.DenoPod, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadPod(ctx, ref)
}

func (p *Provider) readJob(ctx context.Context, ref Ref) (*v1alpha1.DenoJob, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadJob(ctx, ref)
}

func (p *Provider) readTrigger(ctx context.Context, ref Ref) (*v1alpha1.RunTrigger, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadTrigger(ctx, ref)
}

func (p *Provider) readOpenBao(ctx context.Context, ref Ref) (*v1alpha1.OpenBao, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadOpenBao(ctx, ref)
}

func (p *Provider) readEngine(ctx context.Context, ref Ref) (*v1alpha1.PolicyEngine, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadEngine(ctx, ref)
}

func (p *Provider) readWorkflowPod(ctx context.Context, ref Ref) (*v1alpha1.PolicyWorkflowPod, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ReadWorkflowPod(ctx, ref)
}

func (p *Provider) listWorkflowRuns(ctx context.Context, logicalCluster string) ([]v1alpha1.PolicyWorkflowRun, error) {
	r, err := p.readSource()
	if err != nil {
		return nil, err
	}
	return r.ListWorkflowRuns(ctx, logicalCluster)
}

type podWorkflowRunLister interface {
	ListWorkflowRunsForPod(ctx context.Context, logicalCluster, namespace, podName string) ([]v1alpha1.PolicyWorkflowRun, error)
}

// ponytail: the watch cache answers pod-scoped counts from a label index; a reader without one lists and filters. As with listRunsForJob, the namespace is part of the interface so a lister that ignores it cannot silently answer for the whole workspace.
func (p *Provider) listWorkflowRunsForPod(ctx context.Context, logicalCluster, namespace, podName string) ([]v1alpha1.PolicyWorkflowRun, error) {
	if lister, ok := p.reader.(podWorkflowRunLister); ok {
		return lister.ListWorkflowRunsForPod(ctx, logicalCluster, namespace, podName)
	}
	runs, err := p.listWorkflowRuns(ctx, logicalCluster)
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.PolicyWorkflowRun
	for i := range runs {
		if runs[i].Namespace == namespace && runs[i].Labels[v1alpha1.PolicyWorkflowPodLabel] == podName {
			out = append(out, runs[i])
		}
	}
	return out, nil
}

func (p *Provider) Reconcile(ctx context.Context, ref Ref) (Pass, error) {
	run, err := p.readPolicyRun(ctx, ref)
	if err != nil {
		return Pass{}, err
	}
	adm, err := p.admit(ctx, ref, run)
	if err != nil {
		return Pass{}, err
	}
	return p.reconcileWorkflowRun(ctx, ref, run, adm)
}

func (p *Provider) reconcileWorkflowRun(ctx context.Context, ref Ref, run *v1alpha1.PolicyWorkflowRun, adm runAdmission) (Pass, error) {
	pass := Pass{}
	ref.ResourceVersion = run.ResourceVersion
	endpoint := adm.endpoint
	if endpoint == "" {
		endpoint = run.Spec.EngineEndpoint
	}
	workflow := adm.workflow
	if len(run.Spec.Workflow.Raw) > 0 {
		workflow = run.Spec.Workflow.Raw
	}
	inputs := adm.inputs
	if run.Spec.Inputs != nil {
		inputs = run.Spec.Inputs
	}

	o := policyworkflowrun.Observed{
		WorkflowRun:         *run,
		Admission:           adm.admission(),
		EffectiveTTLSeconds: adm.ttl,
		Now:                 p.opts.Now(),
	}
	if !run.Spec.Cancel && run.DeletionTimestamp == nil &&
		run.Status.RunID != "" && endpoint != "" {
		st, err := p.opts.PolicyClient.Status(ctx, endpoint, run.Status.RunID)
		if err != nil {
			return pass, fmt.Errorf("provider: observe policy run %s for %s: %w", run.Status.RunID, ref.Name, err)
		}
		o.Run = &policyworkflowrun.RunObservation{
			RunID:      run.Status.RunID,
			State:      policyworkflowrun.RunState(st.State),
			ExitStatus: st.ExitStatus,
			Outputs:    st.Outputs,
			Message:    st.Message,
		}
	}

	res, err := p.opts.Decider.Reconcile(ctx, o)
	if err != nil {
		return pass, err
	}
	pass.Phase = res.Phase
	pass.RequeueAfter = res.RequeueAfter

	if !p.opts.WriteStatus {
		return pass, nil
	}

	runID := res.RunID
	for _, op := range res.Ops {
		switch op {
		case policyworkflowrun.OpStartRun:
			for _, pre := range adm.preempt {
				if err := p.opts.Registry.Delete(ctx, pre); err != nil {
					return pass, err
				}
			}
			if endpoint == "" {
				return pass, fmt.Errorf("provider: policy run %s has no engine endpoint (admission gated=%v reason=%q pod=%q)",
					ref.Name, adm.gated, adm.reason, runPodName(run))
			}
			id, err := p.opts.PolicyClient.Submit(ctx, endpoint, workflow, inputs)
			if err != nil {
				return pass, fmt.Errorf("provider: submit policy run for %s: %w", ref.Name, err)
			}
			runID = id
		}
	}

	status := v1alpha1.PolicyWorkflowRunStatus{
		RunID:          runID,
		StartTime:      res.StartTime,
		CompletionTime: res.CompletionTime,
		Active:         res.Active,
		Succeeded:      res.Succeeded,
		Failed:         res.Failed,
		Retries:        res.Retries,
		ExitStatus:     res.ExitStatus,
		Outputs:        res.Outputs,
		Conditions:     res.Conditions,
	}
	status.Phase = res.Phase
	if !reflect.DeepEqual(status, run.Status) {
		if err := p.opts.Registry.WriteStatus(ctx, ref, status); err != nil {
			return pass, err
		}
	}

	for _, op := range res.Ops {
		switch op {
		case policyworkflowrun.OpRemoveFinalizer:
			if err := p.opts.Registry.RemoveFinalizer(ctx, ref); err != nil {
				return pass, err
			}
		case policyworkflowrun.OpDelete:
			if err := p.opts.Registry.Delete(ctx, ref); err != nil {
				return pass, err
			}
		}
	}
	if run.Status.Phase == v1alpha1.PolicyWorkflowRunning && res.Phase != v1alpha1.PolicyWorkflowRunning {
		p.wakeQueuedRuns(ctx, ref.LogicalCluster, ref.Namespace, runPodName(run))
	}
	return pass, nil
}

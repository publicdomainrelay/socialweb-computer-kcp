package provider

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/denospec"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/listeners"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/factory/servicenames"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/denopod"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/abc/runner"
)

func (p *Provider) reconcilePod(ctx context.Context, ref Ref, pod *v1alpha1.DenoPod) (time.Duration, bool, error) {
	ref.ResourceVersion = pod.ResourceVersion
	o := denopod.Observed{Pod: *pod, Now: p.opts.Now(), Refusal: refusal(pod)}
	address := ""
	var reported []v1alpha1.DenoPodListener
	if pod.Status.RunID != "" {
		st, err := p.opts.PodRunner.Observe(ctx, pod.Status.RunID)
		if err != nil {
			return 0, false, fmt.Errorf("provider: observe pod run %s for %s: %w", pod.Status.RunID, ref.Name, err)
		}
		state := denopod.ExecutionExited
		if st.State == runner.StateRunning {
			state = denopod.ExecutionRunning
			bound := servicenames.BoundListeners(pod.Spec.Env["SERVICE_ARGS"], pod.Spec.Env["SERVICE_ENV"], st.BoundPort, st.Listeners)
			address = listeners.Primary(bound)
			reported = statusListeners(bound)
		}
		if address != pod.Status.Address || !slices.Equal(reported, pod.Status.Listeners) {
			p.publishListeners(ctx, ref, address, reported)
		}
		o.Execution = &denopod.ExecutionObservation{
			RunID:    pod.Status.RunID,
			State:    state,
			ExitCode: exitCodePtr(st.ExitCode),
			Message:  st.Message,
			Reason:   st.Reason,
			Outputs:  st.Outputs,
		}
		p.observeProbes(ctx, ref, pod, &o)
	}
	res, err := p.opts.PodDecider.Reconcile(ctx, o)
	if err != nil {
		return 0, false, err
	}
	terminal := podTerminalPhase(res.Phase)
	if !p.opts.WriteStatus {
		return res.RequeueAfter, terminal, nil
	}
	if err := p.ensurePodFinalizer(ctx, ref, pod); err != nil {
		return 0, false, err
	}
	var pr projection
	var projectErr error
	if !terminal && pod.DeletionTimestamp == nil {
		pr, projectErr = p.project(ctx, ref, pod)
	}
	runID := res.RunID
	if runID != pod.Status.RunID || terminal {
		if address != "" || pod.Status.Address != "" || len(reported) > 0 || len(pod.Status.Listeners) > 0 {
			p.publishListeners(ctx, ref, "", nil)
		}
		address = ""
		reported = nil
	}
	for _, op := range res.Ops {
		switch op {
		case denopod.OpStopRun:
			if pod.Status.RunID != "" {
				if err := p.opts.PodRunner.Stop(ctx, pod.Status.RunID); err != nil {
					return 0, false, err
				}
			}
		case denopod.OpStartRun:
			if projectErr != nil {
				res.Message = projectErr.Error()
				continue
			}
			req, err := p.podRequest(ctx, ref, &pod.Spec.DenoPodTemplate, podMemory(pod.Spec.Resources), pr)
			if err != nil {
				return 0, false, err
			}
			id, err := p.opts.PodRunner.Start(ctx, req)
			if err != nil {
				return 0, false, fmt.Errorf("provider: start pod %s: %w", ref.Name, err)
			}
			runID = id
		}
	}
	status := v1alpha1.DenoPodStatus{
		RunID:          runID,
		Restarts:       res.Restarts,
		RestartReasons: res.RestartReasons,
		StartTime:      res.StartTime,
		CompletionTime: res.CompletionTime,
		ExitCode:       res.ExitCode,
		Message:        res.Message,
		Outputs:        res.Outputs,
		Ready:          res.Ready,
		Address:        address,
		Listeners:      reported,
		Projected:      pr.status,
		Conditions:     res.Conditions,
	}
	status.Phase = res.Phase
	if !reflect.DeepEqual(status, pod.Status) {
		if err := p.opts.Runtime.WritePodStatus(ctx, ref, status); err != nil {
			if runID != "" && runID != res.RunID {
				_ = p.opts.PodRunner.Stop(ctx, runID)
			}
			return 0, false, err
		}
	}
	if _, err := p.serviceResolver().WriteTableFile(ctx); err != nil {
		p.opts.Log.Warn("kcpdns: the table file was not rewritten, peers keep the addresses they had", "err", err)
	}
	for _, op := range res.Ops {
		switch op {
		case denopod.OpRemoveFinalizer:
			p.removeRunDirs(ctx, p.opts.PodRunner, "DenoPod", ref.Name, pod.Status.RunID, runID)
			p.removeProjected(ref)
			if err := p.opts.Runtime.RemovePodFinalizer(ctx, ref); err != nil {
				return 0, false, err
			}
		case denopod.OpDelete:
			if err := p.opts.Runtime.DeletePod(ctx, ref); err != nil {
				return 0, false, err
			}
		}
	}
	return res.RequeueAfter, terminal, nil
}

type podFinalizerWriter interface {
	WritePodFinalizer(ctx context.Context, ref Ref, finalizers []string) error
}

func (p *Provider) ensurePodFinalizer(ctx context.Context, ref Ref, pod *v1alpha1.DenoPod) error {
	if pod.DeletionTimestamp != nil || hasFinalizer(pod.Finalizers, v1alpha1.FinalizerDenoPod) {
		return nil
	}
	w, ok := p.opts.Runtime.(podFinalizerWriter)
	if !ok {
		return nil
	}
	if err := w.WritePodFinalizer(ctx, ref, append(slices.Clone(pod.Finalizers), v1alpha1.FinalizerDenoPod)); err != nil {
		return fmt.Errorf("provider: the %s finalizer on pod %s: %w", v1alpha1.FinalizerDenoPod, ref.Name, err)
	}
	pod.Finalizers = append(pod.Finalizers, v1alpha1.FinalizerDenoPod)
	return nil
}

func refusal(pod *v1alpha1.DenoPod) string {
	if pod.Annotations[v1alpha1.AnnotationEdge] != "" {
		return ""
	}
	return strings.Join(servicenames.FixedPorts(pod.Spec.Env), ", ")
}

func statusListeners(ls []listeners.Listener) []v1alpha1.DenoPodListener {
	if len(ls) == 0 {
		return nil
	}
	out := make([]v1alpha1.DenoPodListener, 0, len(ls))
	for _, l := range ls {
		out = append(out, v1alpha1.DenoPodListener{Name: l.Name, Kind: l.Kind, Address: l.Address})
	}
	return out
}

func runnerListeners(ls []v1alpha1.DenoPodListener) []listeners.Listener {
	out := make([]listeners.Listener, 0, len(ls))
	for _, l := range ls {
		out = append(out, listeners.Listener{Name: l.Name, Kind: l.Kind, Address: l.Address})
	}
	return out
}

func (p *Provider) observeProbes(ctx context.Context, ref Ref, pod *v1alpha1.DenoPod, o *denopod.Observed) {
	if pod.Spec.ReadinessProbe != nil && len(pod.Spec.ReadinessProbe.Command) > 0 {
		passed := p.runProbe(ctx, pod.Status.RunID, pod.Spec.ReadinessProbe)
		o.ReadinessPassed = &passed
	}
	if pod.Spec.LivenessProbe != nil && len(pod.Spec.LivenessProbe.Command) > 0 {
		passed := p.runProbe(ctx, pod.Status.RunID, pod.Spec.LivenessProbe)
		key := ref.Key()
		o.LivenessFailed = p.probes.Record(key, pod.Status.RunID, passed, thresholdOf(pod.Spec.LivenessProbe))
	}
}

func (p *Provider) runProbe(ctx context.Context, runID string, probe *v1alpha1.ExecProbe) bool {
	timeout := DefaultProbeTimeout
	if probe.TimeoutSeconds != nil && *probe.TimeoutSeconds > 0 {
		timeout = time.Duration(*probe.TimeoutSeconds) * time.Second
	}
	// ponytail: a shim probe that cannot be expanded is skipped rather than run,
	// because PodRunner.Probe treats an empty command as a pass; passing nil
	// through would mark the workload Ready on the strength of having no probe.
	command := p.probeCommand(probe.Command)
	if command == nil {
		p.opts.Log.Warn("kcpdns: a probe names a service the shim cannot resolve, skipping it", "runID", runID)
		return false
	}
	passed, err := p.opts.PodRunner.Probe(ctx, runID, command, timeout)
	if err != nil {
		return false
	}
	return passed
}

func podMemory(r *v1alpha1.DenoPodResources) string {
	if r == nil {
		return ""
	}
	return r.Memory
}

func (p *Provider) podRequest(ctx context.Context, ref Ref, tmpl *v1alpha1.DenoPodTemplate, memory string, pr projection) (runner.PodRequest, error) {
	memoryMax, err := parseMemory(memory)
	if err != nil {
		return runner.PodRequest{}, fmt.Errorf("provider: %s resources.memory: %w", ref.Name, err)
	}
	args, err := denospec.Args(withProjected(tmpl.Permissions.DenoSpec(), pr))
	if err != nil {
		return runner.PodRequest{}, fmt.Errorf("provider: %s permissions: %w", ref.Name, err)
	}
	env := p.podEnv(ctx, ref, tmpl)
	if len(pr.env) > 0 {
		value, err := projectedEnvValue(pr)
		if err != nil {
			return runner.PodRequest{}, err
		}
		env[v1alpha1.ProjectedEnv] = value
	}
	req := runner.PodRequest{
		Name:           ref.Name,
		Service:        p.serviceResolver().Name(ctx, ref.Name, ref.Namespace, ref.LogicalCluster),
		LogicalCluster: ref.LogicalCluster,
		DenoJSON:       string(tmpl.DenoJSON.Raw),
		DenoLock:       tmpl.DenoLock,
		Script:         tmpl.Script,
		PermissionArgs: args,
		Env:            env,
		Server:         p.serverFor(tmpl, ref),
		Workspace:      ref.LogicalCluster,
		MemoryMax:      memoryMax,
	}
	if tmpl.ServiceAccount != nil {
		if p.opts.Minter == nil {
			return runner.PodRequest{}, fmt.Errorf("provider: %s names a service account but the provider has no token minter", ref.Name)
		}
		token, err := p.opts.Minter.MintServiceAccountToken(ctx, ref.LogicalCluster,
			tmpl.ServiceAccount.Namespace, tmpl.ServiceAccount.Name, p.opts.TokenTTL)
		if err != nil {
			return runner.PodRequest{}, err
		}
		req.Token = token
	}
	return req, nil
}

func (p *Provider) serverFor(tmpl *v1alpha1.DenoPodTemplate, target Ref) string {
	if tmpl.APIServer != "" {
		return tmpl.APIServer
	}
	if p.opts.Host == "" {
		return ""
	}
	return ref.BaseHost(p.opts.Host) + "/clusters/" + target.LogicalCluster
}

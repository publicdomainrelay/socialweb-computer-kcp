package denopod

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
)

const RequeueAfter = 2 * time.Second

type ExecutionState string

const (
	ExecutionRunning ExecutionState = "running"

	ExecutionExited ExecutionState = "exited"
)

type ExecutionObservation struct {
	RunID string

	State ExecutionState

	ExitCode *int32

	Message string

	Reason string

	Outputs map[string]string
}

type Observed struct {
	Pod v1alpha1.DenoPod

	Execution *ExecutionObservation

	ReadinessPassed *bool

	LivenessFailed bool

	Refusal string

	Now time.Time
}

type Op string

const (
	OpStartRun Op = "start-run"

	OpStopRun Op = "stop-run"

	OpDelete Op = "delete"

	OpRemoveFinalizer Op = "remove-finalizer"
)

type Result struct {
	Phase v1alpha1.DenoPodPhase

	RunID string

	RunGeneration int64

	Restarts int32

	RestartReasons map[string]int32

	StartTime *metav1.Time

	CompletionTime *metav1.Time

	ExitCode *int32

	Message string

	Outputs map[string]string

	Ready bool

	Conditions []metav1.Condition

	Ops []Op

	RequeueAfter time.Duration

	RemoveFinalizer bool

	Delete bool
}

type Options struct {
	Now func() time.Time
}

type Reconciler struct {
	opts Options
}

func New(opts Options) *Reconciler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Reconciler{opts: opts}
}

func (r *Reconciler) Reconcile(ctx context.Context, o Observed) (Result, error) {
	if o.Pod.Name == "" {
		return Result{}, errors.New("denopod: observed pod has no name")
	}
	if o.Now.IsZero() {
		o.Now = r.opts.Now()
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	res := carry(o.Pod.Status)

	if o.Pod.DeletionTimestamp != nil {
		return r.teardown(o, res), nil
	}

	if terminal(res.Phase) {
		return r.finish(o, res), nil
	}

	if o.Refusal != "" {
		return r.refuse(o, res), nil
	}

	if deadlineExceeded(o.Pod, o.Now) {
		return r.deadline(o, res), nil
	}

	if o.Execution == nil {
		return r.start(o, res), nil
	}

	if o.Execution.State == ExecutionRunning {
		return r.running(o, res), nil
	}

	return r.exited(o, res), nil
}

func carry(st v1alpha1.DenoPodStatus) Result {
	return Result{
		Phase:          st.Phase,
		RunID:          st.RunID,
		RunGeneration:  st.RunGeneration,
		Restarts:       st.Restarts,
		RestartReasons: maps.Clone(st.RestartReasons),
		StartTime:      st.StartTime,
		CompletionTime: st.CompletionTime,
		ExitCode:       st.ExitCode,
		Message:        st.Message,
		Outputs:        st.Outputs,
		Ready:          st.Ready,
		Conditions:     append([]metav1.Condition(nil), st.Conditions...),
	}
}

func (r *Reconciler) teardown(o Observed, res Result) Result {
	if o.Execution != nil && o.Execution.State == ExecutionRunning {
		res.Ops = append(res.Ops, OpStopRun)
	}
	res.Ops = append(res.Ops, OpRemoveFinalizer)
	res.RemoveFinalizer = true
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		"Terminating", "the pod is being deleted")
	res.RequeueAfter = RequeueAfter
	return res
}

func (r *Reconciler) finish(o Observed, res Result) Result {
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		"Terminal", "the pod reached a terminal phase")
	ttl := o.Pod.Spec.TTLSecondsAfterFinished
	if ttl == nil || res.CompletionTime == nil {
		return res
	}
	expiry := res.CompletionTime.Add(time.Duration(*ttl) * time.Second)
	if !o.Now.Before(expiry) {
		res.Delete = true
		res.Ops = append(res.Ops, OpDelete)
		return res
	}
	res.RequeueAfter = expiry.Sub(o.Now)
	return res
}

func (r *Reconciler) deadline(o Observed, res Result) Result {
	if o.Execution != nil && o.Execution.State == ExecutionRunning {
		res.Ops = append(res.Ops, OpStopRun)
	}
	res.Phase = v1alpha1.DenoPodFailed
	res.RunID = ""
	res.Ready = false
	res.CompletionTime = timePtr(o.Now)
	setCondition(&res, o.Pod.Generation, metav1.ConditionTrue, v1alpha1.ConditionFailed,
		"DeadlineExceeded", "activeDeadlineSeconds elapsed before the deno process finished")
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		"DeadlineExceeded", "the pod exceeded its active deadline")
	res.RequeueAfter = RequeueAfter
	return res
}

func (r *Reconciler) refuse(o Observed, res Result) Result {
	if o.Execution != nil && o.Execution.State == ExecutionRunning {
		res.Ops = append(res.Ops, OpStopRun)
	}
	message := "a listener behind the edge binds port 0 and reports its address; this pod names a fixed port (" +
		o.Refusal + ") and carries no " + v1alpha1.AnnotationEdge + " annotation"
	res.Phase = v1alpha1.DenoPodFailed
	res.RunID = ""
	res.Ready = false
	res.Message = message
	res.CompletionTime = timePtr(o.Now)
	setCondition(&res, o.Pod.Generation, metav1.ConditionTrue, v1alpha1.ConditionFailed,
		v1alpha1.ReasonFixedPortBehindTheEdge, message)
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		v1alpha1.ReasonFixedPortBehindTheEdge, message)
	res.RequeueAfter = RequeueAfter
	return res
}

func (r *Reconciler) start(o Observed, res Result) Result {
	if res.StartTime == nil {
		res.StartTime = timePtr(o.Now)
	}
	res.Phase = v1alpha1.DenoPodRunning
	res.RunID = ""
	res.RunGeneration = o.Pod.Generation
	res.Ops = append(res.Ops, OpStartRun)
	res.Ready = false
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		"Starting", "the deno process is starting")
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionComplete,
		"Running", "the deno process is long-running")
	if want := requestedRestart(o.Pod); want != "" {
		setCondition(&res, o.Pod.Generation, metav1.ConditionTrue, v1alpha1.ConditionRestarted,
			v1alpha1.ReasonRestartRequested, want)
	}
	res.RequeueAfter = RequeueAfter
	return res
}

func requestedRestart(pod v1alpha1.DenoPod) string {
	return pod.Annotations[v1alpha1.AnnotationRestart]
}

func honouredRestart(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, v1alpha1.ConditionRestarted); c != nil {
		return c.Message
	}
	return ""
}

func (r *Reconciler) running(o Observed, res Result) Result {
	if res.StartTime == nil {
		res.StartTime = timePtr(o.Now)
	}
	res.Phase = v1alpha1.DenoPodRunning
	res.RunID = o.Execution.RunID

	if o.LivenessFailed {
		return r.restart(o, res, "LivenessProbeFailed", "the liveness probe failed; restarting the deno process")
	}

	if res.RunGeneration == 0 {
		res.RunGeneration = o.Pod.Generation
	}
	if res.RunGeneration != o.Pod.Generation {
		return r.restart(o, res, v1alpha1.ReasonSpecChanged, fmt.Sprintf(
			"the spec changed (generation %d; the run was started from generation %d); replacing the deno process", o.Pod.Generation, res.RunGeneration))
	}

	if want := requestedRestart(o.Pod); want != "" && want != honouredRestart(res.Conditions) {
		res = r.restart(o, res, v1alpha1.ReasonRestartRequested,
			"a restart was requested ("+v1alpha1.AnnotationRestart+"="+want+"); restarting the deno process")
		setCondition(&res, o.Pod.Generation, metav1.ConditionTrue, v1alpha1.ConditionRestarted,
			v1alpha1.ReasonRestartRequested, want)
		return res
	}

	ready := true
	if o.ReadinessPassed != nil {
		ready = *o.ReadinessPassed
	}
	res.Ready = ready
	if ready {
		setCondition(&res, o.Pod.Generation, metav1.ConditionTrue, v1alpha1.ConditionReady,
			"Ready", "the deno process is running and ready")
	} else {
		setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
			"NotReady", "the readiness probe has not passed")
	}
	res.RequeueAfter = RequeueAfter
	return res
}

func (r *Reconciler) restart(o Observed, res Result, reason, message string) Result {
	if o.Execution != nil && o.Execution.State == ExecutionRunning {
		res.Ops = append(res.Ops, OpStopRun)
	}
	res.Phase = v1alpha1.DenoPodRunning
	res.RunID = ""
	res.Restarts = o.Pod.Status.Restarts + 1
	if res.RestartReasons == nil {
		res.RestartReasons = map[string]int32{}
	}
	res.RestartReasons[reason]++
	res.Ready = false
	res.Message = message
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		reason, message)
	res.RequeueAfter = RequeueAfter
	return res
}

func (r *Reconciler) exited(o Observed, res Result) Result {
	exit := o.Execution.ExitCode
	success := exit != nil && *exit == 0
	policy := o.Pod.Spec.RestartPolicy
	if policy == "" {
		policy = v1alpha1.RestartAlways
	}

	switch policy {
	case v1alpha1.RestartAlways:
		reason, message := exitReason(o, "the deno process exited; the restart policy is Always")
		return r.restart(o, res, reason, message)
	case v1alpha1.RestartOnFailure:
		if success {
			return r.complete(o, res, true, "Complete", "the deno process exited successfully")
		}
		reason, message := exitReason(o, "the deno process failed; the restart policy is OnFailure")
		return r.restart(o, res, reason, message)
	default:
		if success {
			return r.complete(o, res, true, "Complete", "the deno process exited successfully")
		}
		return r.complete(o, res, false, "ProcessFailed", "the deno process failed and the restart policy is Never")
	}
}

func exitReason(o Observed, message string) (string, string) {
	if o.Execution.Reason == "" {
		return "Restarting", message
	}
	return o.Execution.Reason, o.Execution.Message
}

func (r *Reconciler) complete(o Observed, res Result, ok bool, reason, message string) Result {
	res.RunID = o.Execution.RunID
	res.ExitCode = o.Execution.ExitCode
	res.Message = o.Execution.Message
	res.Outputs = o.Execution.Outputs
	res.CompletionTime = timePtr(o.Now)
	res.Ready = false
	if ok {
		res.Phase = v1alpha1.DenoPodSucceeded
	} else {
		res.Phase = v1alpha1.DenoPodFailed
	}
	setCondition(&res, o.Pod.Generation, metav1.ConditionFalse, v1alpha1.ConditionReady,
		reason, message)
	condition := metav1.ConditionFalse
	if ok {
		condition = metav1.ConditionTrue
	}
	setCondition(&res, o.Pod.Generation, condition, v1alpha1.ConditionComplete,
		reason, message)
	res.RequeueAfter = RequeueAfter
	return res
}

func terminal(p v1alpha1.DenoPodPhase) bool {
	return p == v1alpha1.DenoPodSucceeded || p == v1alpha1.DenoPodFailed
}

func deadlineExceeded(pod v1alpha1.DenoPod, now time.Time) bool {
	if pod.Spec.ActiveDeadlineSeconds == nil || pod.Status.StartTime == nil {
		return false
	}
	deadline := pod.Status.StartTime.Add(time.Duration(*pod.Spec.ActiveDeadlineSeconds) * time.Second)
	return !now.Before(deadline)
}

func timePtr(t time.Time) *metav1.Time {
	v := metav1.NewTime(t)
	return &v
}

func setCondition(res *Result, generation int64, status metav1.ConditionStatus, conditionType, reason, message string) {
	meta.SetStatusCondition(&res.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

package tenantapply

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
)

const (
	DefaultFieldManager = "socialweb-computer"
	DefaultNamespace    = "default"
	LogLimitBytes       = 64 << 10
	RequestTimeout      = 30 * time.Second
)

type OwnerConfig func(ctx context.Context, did, cluster string) (*rest.Config, error)

type Client struct {
	owner OwnerConfig
}

var _ gateway.TenantAPI = (*Client)(nil)

func New(owner OwnerConfig) (*Client, error) {
	if owner == nil {
		return nil, errors.New("tenantapply: no owner credential source")
	}
	return &Client{owner: owner}, nil
}

func invalid(format string, args ...any) error {
	return &gateway.XRPCError{Name: "InvalidRequest", Message: fmt.Sprintf(format, args...), Status: 400}
}

func (c *Client) config(ctx context.Context, did, cluster string) (*rest.Config, error) {
	cfg, err := c.owner(ctx, did, cluster)
	if err != nil {
		return nil, &gateway.XRPCError{
			Name: "ClusterUnavailable", Message: "no credential for the cluster owner: " + oneLine(err.Error()), Status: 503,
		}
	}
	cfg = rest.CopyConfig(cfg)
	cfg.Timeout = RequestTimeout
	cfg.UserAgent = "socialweb-computer-gateway"
	return cfg, nil
}

func (c *Client) Apply(ctx context.Context, did, cluster string, req gateway.ApplyRequest) (*gateway.ApplyResponse, error) {
	obj := &unstructured.Unstructured{}
	if err := obj.UnmarshalJSON(req.Manifest); err != nil {
		return nil, invalid("the manifest is not a Kubernetes object: %s", oneLine(err.Error()))
	}
	gvk := obj.GroupVersionKind()
	switch {
	case gvk.Version == "" || gvk.Kind == "":
		return nil, invalid("the manifest needs apiVersion and kind")
	case strings.HasSuffix(gvk.Kind, "List"):
		return nil, invalid("one object per call; %s is a list", gvk.Kind)
	case obj.GetName() == "":
		return nil, invalid("the manifest needs metadata.name (server-side apply does not take generateName)")
	case req.Name != "" && req.Name != obj.GetName():
		return nil, invalid("name %q does not match metadata.name %q", req.Name, obj.GetName())
	}

	cfg, err := c.config(ctx, did, cluster)
	if err != nil {
		return nil, err
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil, &gateway.XRPCError{Name: "KindNotFound", Message: oneLine(err.Error()), Status: 400}
		}
		return nil, kubeError(err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	var target dynamic.ResourceInterface = dyn.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		switch {
		case ns == "" && req.Namespace != "":
			ns = req.Namespace
		case ns == "":
			ns = DefaultNamespace
		case req.Namespace != "" && req.Namespace != ns:
			return nil, invalid("namespace %q does not match metadata.namespace %q", req.Namespace, ns)
		}
		obj.SetNamespace(ns)
		target = dyn.Resource(mapping.Resource).Namespace(ns)
	} else {
		if req.Namespace != "" || obj.GetNamespace() != "" {
			return nil, invalid("%s is cluster-scoped and takes no namespace", gvk.Kind)
		}
	}

	manager := req.FieldManager
	manager = cmp.Or(manager, DefaultFieldManager)
	opts := metav1.ApplyOptions{FieldManager: manager, Force: req.Force}
	if req.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := target.Apply(ctx, obj.GetName(), obj, opts)
	if err != nil {
		return nil, kubeError(err)
	}
	raw, err := out.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return &gateway.ApplyResponse{
		APIVersion:      out.GetAPIVersion(),
		Kind:            out.GetKind(),
		Namespace:       out.GetNamespace(),
		Name:            out.GetName(),
		UID:             string(out.GetUID()),
		ResourceVersion: out.GetResourceVersion(),
		Generation:      out.GetGeneration(),
		DryRun:          req.DryRun,
		Object:          json.RawMessage(raw),
	}, nil
}

func (c *Client) Job(ctx context.Context, did, cluster string, req gateway.JobRequest) (*gateway.JobStatus, error) {
	cfg, err := c.config(ctx, did, cluster)
	if err != nil {
		return nil, err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	job, err := kube.BatchV1().Jobs(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, kubeError(err)
	}
	st := &gateway.JobStatus{
		Namespace:  job.Namespace,
		Name:       job.Name,
		UID:        string(job.UID),
		Phase:      jobPhase(job),
		Active:     int64(job.Status.Active),
		Succeeded:  int64(job.Status.Succeeded),
		Failed:     int64(job.Status.Failed),
		Conditions: []gateway.JobCondition{},
		Pods:       []gateway.JobPod{},
	}
	if t := job.Status.StartTime; t != nil {
		st.StartTime = t.UTC().Format(time.RFC3339)
	}
	if t := job.Status.CompletionTime; t != nil {
		st.CompletionTime = t.UTC().Format(time.RFC3339)
	}
	for _, cond := range job.Status.Conditions {
		st.Conditions = append(st.Conditions, gateway.JobCondition{
			Type: string(cond.Type), Status: string(cond.Status), Reason: cond.Reason, Message: cond.Message,
		})
	}
	if job.Spec.Selector == nil {
		return st, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(job.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := kube.CoreV1().Pods(req.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, kubeError(err)
	}
	tail := int64(gateway.DefaultJobTailLines)
	if req.TailLines != nil {
		tail = *req.TailLines
	}
	for _, p := range pods.Items {
		if !ownedBy(p.OwnerReferences, job.UID) {
			continue
		}
		jp := gateway.JobPod{Name: p.Name, Phase: string(p.Status.Phase), Node: p.Spec.NodeName, Containers: []gateway.JobContainer{}}
		for _, cs := range p.Status.ContainerStatuses {
			jc := containerState(cs)
			if tail > 0 && (cs.State.Running != nil || cs.State.Terminated != nil) {
				jc.Log, jc.LogError = podLog(ctx, kube, p.Namespace, p.Name, cs.Name, tail)
			}
			jp.Containers = append(jp.Containers, jc)
		}
		st.Pods = append(st.Pods, jp)
	}
	return st, nil
}

func jobPhase(job *batchv1.Job) string {
	for _, cond := range job.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			return gateway.JobSucceeded
		case batchv1.JobFailed, batchv1.JobFailureTarget:
			return gateway.JobFailed
		}
	}
	if job.Status.Active > 0 {
		return gateway.JobRunning
	}
	return gateway.JobPending
}

func ownedBy(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}

func containerState(cs corev1.ContainerStatus) gateway.JobContainer {
	jc := gateway.JobContainer{Name: cs.Name, State: "Waiting"}
	switch {
	case cs.State.Terminated != nil:
		code := int64(cs.State.Terminated.ExitCode)
		jc.State, jc.Reason, jc.ExitCode = "Terminated", cs.State.Terminated.Reason, &code
	case cs.State.Running != nil:
		jc.State = "Running"
	case cs.State.Waiting != nil:
		jc.Reason = cs.State.Waiting.Reason
	}
	return jc
}

func podLog(ctx context.Context, kube kubernetes.Interface, ns, pod, container string, tail int64) (string, string) {
	limit := int64(LogLimitBytes)
	rc, err := kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: container, TailLines: &tail, LimitBytes: &limit,
	}).Stream(ctx)
	if err != nil {
		return "", oneLine(err.Error())
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, LogLimitBytes))
	if err != nil {
		return string(b), oneLine(err.Error())
	}
	return string(b), ""
}

func kubeError(err error) error {
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return &gateway.XRPCError{Name: "ClusterUnavailable", Message: "the cluster did not answer: " + oneLine(err.Error()), Status: 503}
	}
	s := st.Status()
	code := int(s.Code)
	name := "KubernetesError"
	switch {
	case apierrors.IsConflict(err):
		name = "ApplyConflict"
	case apierrors.IsNotFound(err):
		name = "ObjectNotFound"
	}
	if code < 400 || code > 599 {
		code = 502
	}
	return &gateway.XRPCError{Name: name, Message: oneLine(s.Message), Status: code}
}

const maxMessage = 2000

func oneLine(msg string) string {
	line := strings.Join(strings.Fields(msg), " ")
	if len(line) > maxMessage {
		line = line[:maxMessage] + "..."
	}
	return line
}

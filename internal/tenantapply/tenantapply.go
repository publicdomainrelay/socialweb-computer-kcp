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
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
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
	obj := kube.Doc{}
	if err := utiljson.Unmarshal(req.Manifest, &obj); err != nil {
		return nil, invalid("the manifest is not a Kubernetes object: %s", oneLine(err.Error()))
	}
	gvk := groupVersionKind(obj)
	if gvk.Kind == "" {
		return nil, invalid("the manifest is not a Kubernetes object: %s", oneLine(kruntime.NewMissingKindErr(string(req.Manifest)).Error()))
	}
	name := docString(obj, "metadata", "name")
	switch {
	case gvk.Version == "":
		return nil, invalid("the manifest needs apiVersion and kind")
	case strings.HasSuffix(gvk.Kind, "List"):
		return nil, invalid("one object per call; %s is a list", gvk.Kind)
	case name == "":
		return nil, invalid("the manifest needs metadata.name (server-side apply does not take generateName)")
	case req.Name != "" && req.Name != name:
		return nil, invalid("name %q does not match metadata.name %q", req.Name, name)
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

	kc, err := kube.New(cfg)
	if err != nil {
		return nil, err
	}
	target := kc.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := docString(obj, "metadata", "namespace")
		switch {
		case ns == "" && req.Namespace != "":
			ns = req.Namespace
		case ns == "":
			ns = DefaultNamespace
		case req.Namespace != "" && req.Namespace != ns:
			return nil, invalid("namespace %q does not match metadata.namespace %q", req.Namespace, ns)
		}
		obj["metadata"].(map[string]any)["namespace"] = ns
		target = target.In(ns)
	} else {
		if req.Namespace != "" || docString(obj, "metadata", "namespace") != "" {
			return nil, invalid("%s is cluster-scoped and takes no namespace", gvk.Kind)
		}
	}

	manager := req.FieldManager
	manager = cmp.Or(manager, DefaultFieldManager)
	opts := kube.ApplyOptions{FieldManager: manager, Force: req.Force}
	if req.DryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := kube.Apply[kube.Doc](ctx, target, obj, opts)
	if err != nil {
		return nil, kubeError(err)
	}
	raw, err := json.Marshal(*out)
	if err != nil {
		return nil, err
	}
	generation, _ := docField(*out, "metadata", "generation").(int64)
	return &gateway.ApplyResponse{
		APIVersion:      docString(*out, "apiVersion"),
		Kind:            docString(*out, "kind"),
		Namespace:       docString(*out, "metadata", "namespace"),
		Name:            docString(*out, "metadata", "name"),
		UID:             docString(*out, "metadata", "uid"),
		ResourceVersion: docString(*out, "metadata", "resourceVersion"),
		Generation:      generation,
		DryRun:          req.DryRun,
		Object:          json.RawMessage(raw),
	}, nil
}

func docField(d kube.Doc, fields ...string) any {
	var v any = map[string]any(d)
	for _, f := range fields {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[f]
	}
	return v
}

func docString(d kube.Doc, fields ...string) string {
	s, _ := docField(d, fields...).(string)
	return s
}

func groupVersionKind(d kube.Doc) schema.GroupVersionKind {
	gv, err := schema.ParseGroupVersion(docString(d, "apiVersion"))
	if err != nil {
		return schema.GroupVersionKind{}
	}
	return gv.WithKind(docString(d, "kind"))
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

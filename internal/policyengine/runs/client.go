package runs

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	ModeScope = "scope"

	ModeFull = "full"

	TTLSeconds = int64(3600)

	DefaultTimeout = 2 * time.Minute
)

type (
	Input = policy.EvaluationInput

	Request = policy.Evaluation
)

var _ policy.Evaluator = (*Client)(nil)

type ResolveFunc func(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error)

type Client struct {
	Resolve ResolveFunc

	Owner policy.Owner

	Engine commonv1.ObjectRef

	Role string

	Prefix string

	Args map[string]string

	Timeout time.Duration

	Parser Parser

	Now func() time.Time

	counter atomic.Uint64
}

func EngineRef(ref *commonv1.ObjectRef, workspace string) commonv1.ObjectRef {
	if ref == nil || strings.TrimSpace(ref.Name) == "" {
		return commonv1.ObjectRef{}
	}
	return commonv1.ObjectRef{Group: policyv1alpha1.GroupName, Kind: "PolicyEngine", Name: strings.TrimSpace(ref.Name),
		Workspace: ref.InWorkspace(workspace)}
}

func (c *Client) Run(ctx context.Context, req Request) Decision {
	if c == nil || c.Resolve == nil {
		return Decision{Reason: "the policy run client is not configured"}
	}
	if c.Engine.Name == "" {
		return Decision{Reason: "no PolicyEngine is configured for this " + c.Role}
	}
	workflow := cmp.Or(req.WorkflowRef, req.Source)
	if workflow == "" || (req.WorkflowRef == "" && len(req.Workflow) == 0) {
		return Decision{Reason: "the policy run names no workflow"}
	}
	timeout := opt.Positive(req.Timeout, c.Timeout)
	timeout = opt.Positive(timeout, DefaultTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner, err := c.Resolve(runCtx, c.Engine, workflow)
	if err != nil {
		return Decision{Reason: "the policy run could not be created: " + err.Error()}
	}
	name, err := runner.Run(runCtx, policy.RunRequest{Name: c.runName(), Workflow: workflow, Owner: c.Owner, Spec: c.Spec(req)})
	if err != nil {
		return Decision{Reason: "the policy run could not be created: " + err.Error()}
	}
	status, err := runner.Wait(runCtx, name)
	if err != nil {
		return Decision{Reason: "the policy run did not finish: " + err.Error(), Run: name}
	}
	return Decide(&policyv1alpha1.PolicyWorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: status}, c.Parser)
}

func (c *Client) Spec(req Request) policyv1alpha1.PolicyWorkflowRunSpec {
	input := req.Input
	input.Mode = cmp.Or(input.Mode, ModeFull)
	inputs := map[string]string{"mode": input.Mode}
	for key, value := range map[string]string{"self-did": input.SelfDid, "perspective": input.Perspective, "subject-did": input.SubjectDid,
		"counterparty-did": input.CounterpartyDid, "root-requester-did": input.RootRequesterDid} {
		if value != "" {
			inputs[key] = value
		}
	}
	args := map[string]string{}
	maps.Copy(args, c.Args)
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte("{}")
	}
	inputs["policy-args"] = string(raw)
	deadline := int64(opt.Positive(req.Timeout, c.Timeout) / time.Second)
	if deadline <= 0 {
		deadline = int64(DefaultTimeout / time.Second)
	}
	ttl := TTLSeconds
	return policyv1alpha1.PolicyWorkflowRunSpec{
		WorkflowRef:             req.WorkflowRef,
		Workflow:                append(json.RawMessage(nil), req.Workflow...),
		Inputs:                  inputs,
		Args:                    args,
		Perspective:             input.Perspective,
		SelfDid:                 input.SelfDid,
		SubjectDid:              input.SubjectDid,
		CounterpartyDid:         input.CounterpartyDid,
		RootRequesterDid:        input.RootRequesterDid,
		RFP:                     strongRef(input.RFP),
		Bid:                     strongRef(input.Bid),
		Accept:                  strongRef(input.Accept),
		ActiveDeadlineSeconds:   &deadline,
		TTLSecondsAfterFinished: &ttl,
	}
}

func (c *Client) runName() string {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return fmt.Sprintf("%s-%d-%d", cmp.Or(c.Prefix, "policy"), now().UnixNano(), c.counter.Add(1))
}

func strongRef(ref *commonv1.StrongRef) *commonv1.StrongRef {
	if ref == nil || ref.URI == "" {
		return nil
	}
	return &commonv1.StrongRef{URI: ref.URI, CID: ref.CID}
}

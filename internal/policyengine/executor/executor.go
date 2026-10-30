package executor

import (
	"context"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/bundled-actions"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/trust"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/workflow"
)

const (
	PolicyGhaLiteNSID     = "computer.socialweb.temp.policy.ghalite"
	PolicyTypescriptNSID  = "computer.socialweb.temp.policy.typescript"
	PolicyDenoWorkerNSID  = "com.publicdomainrelay.temp.market.policies.denoWorker"
	SandboxDeniedPolicyID = "sandbox-denied"
	ExpressionDenied      = "the policy expression was denied"
	NetOnlyRefused        = "this evaluator runs gha-lite policies net-only and refuses"
)

type Perspective string

const (
	PerspectiveBidder    Perspective = "bidder"
	PerspectiveRequester Perspective = "requester"
)

type PolicyRecord struct {
	URI     string         `json:"uri"`
	CID     string         `json:"cid"`
	Value   map[string]any `json:"value,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

func (r PolicyRecord) Ref() atproto.StrongRef {
	return atproto.StrongRef{URI: r.URI, CID: r.CID}
}

type Violation struct {
	Msg      string `json:"msg"`
	PolicyID any    `json:"policyId"`
}

type Verdict struct {
	Allow      bool        `json:"allow"`
	Violations []Violation `json:"violations"`
}

func Allow() Verdict {
	return Verdict{Allow: true, Violations: []Violation{}}
}

func Deny(msg string, policyID any) Verdict {
	return Verdict{Allow: false, Violations: []Violation{{Msg: msg, PolicyID: policyID}}}
}

func RejectionError(verdict Verdict) string {
	for _, violation := range verdict.Violations {
		if value, ok := violation.PolicyID.(string); ok && value == SandboxDeniedPolicyID {
			return "policy sandbox denied"
		}
	}
	return "policy rejected"
}

type EvalContext struct {
	PolicyName       string
	Args             map[string]any
	Perspective      Perspective
	SelfDid          string
	SubjectDid       string
	RootRequesterDid string
	CounterpartyDid  string
	PolicyRef        *atproto.StrongRef
	RFP              *PolicyRecord
	Bid              *PolicyRecord
	Accept           *PolicyRecord
	Cache            models.Cache
	Stack            map[string]any
	Host             trust.Host
}

type ScopeInput struct {
	Perspective           Perspective
	SelfDid               string
	CounterpartyDid       string
	Args                  map[string]any
	ResolveOperatorDid    func(context.Context, string) (string, bool, error)
	ResolveOperatorDidFor func(context.Context, string, string) (string, bool, error)
	Vouches               func(context.Context, string, string) (bool, error)
}

type Executor interface {
	Kind() string
	Execute(ctx context.Context, record PolicyRecord, eval *EvalContext, permissions map[string]any) (Verdict, error)
	Scope(ctx context.Context, record PolicyRecord, input ScopeInput) (Verdict, bool, error)
}

func RecordType(uri string, value map[string]any) string {
	if value != nil {
		if kind, ok := value["$type"].(string); ok && kind != "" {
			return kind
		}
	}
	_, collection, _ := atproto.SplitURI(uri)
	return collection
}

func BundledActions(uses string) (*workflow.Action, error) {
	action, err := bundledactions.Lookup(uses)
	if err != nil {
		return nil, err
	}
	inputs := make(map[string]string, len(action.Inputs))
	for name, def := range action.Inputs {
		inputs[name] = def.Default
	}
	return &workflow.Action{
		Uses:   action.Uses,
		Path:   action.Path,
		Using:  action.Using,
		Main:   action.Main,
		Inputs: inputs,
		Source: action.Source,
	}, nil
}

func DefaultActionResolver() workflow.ActionResolver {
	return BundledActions
}

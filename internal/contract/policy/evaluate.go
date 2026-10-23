package policy

import (
	"context"
	"encoding/json"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type EvaluationInput struct {
	Perspective string

	Mode string

	SelfDid string

	SubjectDid string

	CounterpartyDid string

	RootRequesterDid string

	RFP *commonv1.StrongRef

	Bid *commonv1.StrongRef

	Accept *commonv1.StrongRef
}

type Evaluation struct {
	WorkflowRef string

	Workflow json.RawMessage

	Source string

	Input EvaluationInput

	Timeout time.Duration
}

type Verdict struct {
	Allow bool

	Violations []string

	Reason string

	Run string
}

type Evaluator interface {
	Run(ctx context.Context, req Evaluation) Verdict
}

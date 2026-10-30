package participant

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	PolicyModeScope = runs.ModeScope

	PolicyModeFull = runs.ModeFull
)

type PolicyDecision = runs.Decision

type PolicyRef struct {
	URI string

	CID string
}

func (r *PolicyRef) StrongRef() *commonv1.StrongRef {
	if r == nil || r.URI == "" {
		return nil
	}
	return &commonv1.StrongRef{URI: r.URI, CID: r.CID}
}

func (r PolicyRef) Record() market.RecordRef {
	return market.RecordRef{URI: r.URI, CID: r.CID}
}

type PolicyInput struct {
	Perspective string

	Workflow string

	SelfDid string

	SubjectDid string

	CounterpartyDid string

	RootRequesterDid string

	RFP *PolicyRef

	Bid *PolicyRef

	Accept *PolicyRef

	Mode string
}

func (in PolicyInput) Runs() runs.Input {
	return runs.Input{Perspective: in.Perspective, Mode: in.Mode, SelfDid: in.SelfDid, SubjectDid: in.SubjectDid,
		CounterpartyDid: in.CounterpartyDid, RootRequesterDid: in.RootRequesterDid,
		RFP: in.RFP.StrongRef(), Bid: in.Bid.StrongRef(), Accept: in.Accept.StrongRef()}
}

type PolicyConfig struct {
	Resolve runs.ResolveFunc

	Owner policy.Owner

	Engine commonv1.ObjectRef

	Role string

	Prefix string

	Args map[string]string

	Timeout time.Duration

	Parser runs.Parser

	Association *commonv1.AssociationPolicy
}

func NewPolicyRuns(cfg PolicyConfig) *runs.Client {
	if cfg.Resolve == nil {
		return nil
	}
	return &runs.Client{Resolve: cfg.Resolve, Owner: cfg.Owner, Engine: cfg.Engine, Role: cfg.Role, Prefix: cfg.Prefix,
		Args: cfg.Args, Timeout: cfg.Timeout, Parser: cfg.Parser, Association: cfg.Association.DeepCopy(), Now: time.Now}
}

func ParsePolicyArgs(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]string{}, nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, fmt.Errorf("participant: policyArgs is not JSON: %w", err)
	}
	if value == nil {
		return map[string]string{}, nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("participant: policyArgs must be a JSON object")
	}
	out := make(map[string]string, len(obj))
	for key, item := range obj {
		switch typed := item.(type) {
		case string:
			out[key] = typed
		case bool:
			out[key] = fmt.Sprintf("%t", typed)
		case float64:
			out[key] = fmt.Sprintf("%v", typed)
		default:
			data, err := json.Marshal(typed)
			if err != nil {
				return nil, err
			}
			out[key] = string(data)
		}
	}
	return out, nil
}

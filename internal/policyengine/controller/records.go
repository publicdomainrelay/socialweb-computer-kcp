package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/cache"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/executor"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/models"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/registry"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type RecordCacheSource interface {
	ResolveRecordCached(ctx context.Context, ref commonv1.StrongRef, seed models.Cache) (map[string]any, error)

	ResolveWorkflowCached(ctx context.Context, ref commonv1.StrongRef, seed models.Cache) (json.RawMessage, error)
}

type RecordResolver struct {
	ResolveFunc func(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error)

	Registry *registry.Registry
}

func NewRecordResolver(resolve func(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error), reg *registry.Registry) *RecordResolver {
	return &RecordResolver{ResolveFunc: resolve, Registry: reg}
}

func (r *RecordResolver) ResolveRecord(ctx context.Context, ref commonv1.StrongRef) (map[string]any, error) {
	if r == nil || r.ResolveFunc == nil {
		return nil, errors.New("the policy record resolver is not configured")
	}
	if strings.TrimSpace(ref.URI) == "" {
		return nil, errors.New("the policy record reference has no uri")
	}
	value, err := r.ResolveFunc(ctx, ref)
	if err != nil {
		return nil, err
	}
	if value == nil {
		value = map[string]any{}
	}
	return value, nil
}

func (r *RecordResolver) ResolveRecordCached(ctx context.Context, ref commonv1.StrongRef, seed models.Cache) (map[string]any, error) {
	if value, ok := cache.ReadRecordCache(seed, atproto.StrongRef{URI: ref.URI, CID: ref.CID}); ok {
		return value, nil
	}
	return r.ResolveRecord(ctx, ref)
}

func (r *RecordResolver) ResolveWorkflow(ctx context.Context, ref commonv1.StrongRef) (json.RawMessage, error) {
	value, err := r.ResolveRecord(ctx, ref)
	if err != nil {
		return nil, err
	}
	return r.workflowOfRecord(ref, value)
}

func (r *RecordResolver) ResolveWorkflowCached(ctx context.Context, ref commonv1.StrongRef, seed models.Cache) (json.RawMessage, error) {
	value, err := r.ResolveRecordCached(ctx, ref, seed)
	if err != nil {
		return nil, err
	}
	return r.workflowOfRecord(ref, value)
}

func (r *RecordResolver) workflowOfRecord(ref commonv1.StrongRef, value map[string]any) (json.RawMessage, error) {
	kind := executor.RecordType(ref.URI, value)
	if r.Registry != nil {
		item, ok := r.Registry.Get(kind)
		if !ok {
			return nil, fmt.Errorf("unknown policy record type: %s", kind)
		}
		if deny, ok := item.(*executor.DenyExecutor); ok {
			return nil, errors.New(deny.Message)
		}
	}
	if kind != executor.PolicyGhaLiteNSID {
		return nil, fmt.Errorf("policy record type %s is not gha-lite", kind)
	}
	workflowValue, ok := value["workflow"]
	if !ok || workflowValue == nil {
		return nil, errors.New("the gha-lite policy record has no workflow")
	}
	if text, ok := workflowValue.(string); ok {
		if strings.TrimSpace(text) == "" {
			return nil, errors.New("the gha-lite policy record has an empty workflow")
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
	encoded, err := json.Marshal(workflowValue)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 {
		return nil, errors.New("the gha-lite policy record has an empty workflow")
	}
	return encoded, nil
}

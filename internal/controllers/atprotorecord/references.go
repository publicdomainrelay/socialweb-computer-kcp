package atprotorecord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/strs"
)

type fieldStep struct {
	name    string
	indexes []int
}

func resolveReferences(ctx context.Context, refs References, raw json.RawMessage) (json.RawMessage, []string, error) {
	var value any
	if len(raw) == 0 {
		return nil, []string{"the record is empty"}, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, nil, fmt.Errorf("the record is not valid JSON: %w", err)
	}
	resolved, missing := resolveValue(ctx, refs, value)
	if len(missing) > 0 {
		return nil, missing, nil
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, nil, err
	}
	return out, nil, nil
}

func resolveValue(ctx context.Context, refs References, value any) (any, []string) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 1 {
			if raw, ok := typed["valueFrom"]; ok {
				from, err := decodeValueFrom(raw)
				if err != nil {
					return nil, []string{err.Error()}
				}
				return resolveReference(ctx, refs, from)
			}
		}
		out := make(map[string]any, len(typed))
		var missing []string
		for key, child := range typed {
			resolved, bad := resolveValue(ctx, refs, child)
			if len(bad) > 0 {
				missing = append(missing, bad...)
				continue
			}
			out[key] = resolved
		}
		return out, missing
	case []any:
		out := make([]any, len(typed))
		var missing []string
		for i, child := range typed {
			resolved, bad := resolveValue(ctx, refs, child)
			if len(bad) > 0 {
				missing = append(missing, bad...)
				continue
			}
			out[i] = resolved
		}
		return out, missing
	default:
		return value, nil
	}
}

func decodeValueFrom(raw any) (v1alpha1.AtprotoValueFrom, error) {
	body, err := json.Marshal(raw)
	if err != nil {
		return v1alpha1.AtprotoValueFrom{}, err
	}
	var from v1alpha1.AtprotoValueFrom
	if err := json.Unmarshal(body, &from); err != nil {
		return v1alpha1.AtprotoValueFrom{}, fmt.Errorf("the valueFrom reference: %w", err)
	}
	return from, nil
}

func resolveReference(ctx context.Context, refs References, from v1alpha1.AtprotoValueFrom) (any, []string) {
	label := referenceLabel(from)
	if strings.TrimSpace(from.Name) == "" {
		return nil, []string{label + ": the reference names no object"}
	}
	path := parseFieldPath(from.FieldPath)
	if len(path) == 0 {
		return nil, []string{label + ": the reference names no fieldPath"}
	}
	object, err := readReference(ctx, refs, from)
	if err != nil {
		return nil, []string{label + ": " + err.Error()}
	}
	value, ok := fieldValue(object, path)
	if !ok {
		return nil, []string{label + ": the field " + from.FieldPath + " is missing"}
	}
	return value, nil
}

func readReference(ctx context.Context, refs References, from v1alpha1.AtprotoValueFrom) (map[string]any, error) {
	if refs.Own != nil {
		if object, ok, err := refs.Own(ctx, from); ok {
			return object, err
		}
	}
	client := refs.Dynamic
	if target := strings.TrimSpace(from.Workspace); target != "" && target != refs.Workspace {
		if refs.Factory == nil {
			return nil, errors.New("the reference names another workspace and no dynamic factory is configured")
		}
		var err error
		if client, err = refs.Factory(target); err != nil {
			return nil, err
		}
	}
	if client == nil {
		return nil, errors.New("no dynamic client")
	}
	resource := client.Resource(ReferenceResource(from))
	var getter dynamic.ResourceInterface = resource
	if namespace := strings.TrimSpace(from.Namespace); namespace != "" {
		getter = resource.Namespace(namespace)
	}
	object, err := getter.Get(ctx, strings.TrimSpace(from.Name), metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return object.Object, nil
}

func ReadOwn(ctx context.Context, req ctlruntime.Request, from v1alpha1.AtprotoValueFrom) (map[string]any, bool, error) {
	workspace := strings.TrimSpace(from.Workspace)
	if (workspace != "" && workspace != req.Workspace) || strings.TrimSpace(from.Namespace) != "" || ReferenceResource(from).Group != v1alpha1.GroupName {
		return nil, false, nil
	}
	kind := ReferenceKind(from)
	var obj ctlruntime.Object
	switch kind {
	case MarketIdentityKind:
		obj = &v1alpha1.MarketIdentity{}
	case Kind:
		obj = &v1alpha1.AtprotoRecord{}
	case RecordWatchKind:
		obj = &v1alpha1.AtprotoRecordWatch{}
	default:
		return nil, false, nil
	}
	if err := req.Get(ctx, "", strings.TrimSpace(from.Name), obj); err != nil {
		return nil, true, err
	}
	obj.GetObjectKind().SetGroupVersionKind(v1alpha1.GroupVersion.WithKind(kind))
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	return object, true, err
}

func ReferenceKind(from v1alpha1.AtprotoValueFrom) string {
	if kind := strings.TrimSpace(from.Kind); kind != "" {
		for _, known := range OwnKinds {
			if strings.EqualFold(known, kind) {
				return known
			}
		}
		return kind
	}
	resource := strings.TrimSpace(from.Resource)
	for _, known := range OwnKinds {
		if strings.EqualFold(strs.Pluralize(known), resource) {
			return known
		}
	}
	return resource
}

func ReferenceKey(workspace string, from v1alpha1.AtprotoValueFrom) string {
	return binding.Key(cmp.Or(strings.TrimSpace(from.Workspace), workspace), ReferenceKind(from), strings.TrimSpace(from.Name))
}

func referenceKeys(workspace string, raw json.RawMessage) []string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch typed := v.(type) {
		case map[string]any:
			if len(typed) == 1 {
				if raw, ok := typed["valueFrom"]; ok {
					if from, err := decodeValueFrom(raw); err == nil && strings.TrimSpace(from.Name) != "" {
						out = append(out, ReferenceKey(workspace, from))
					}
					return
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return out
}

func referenceLabel(from v1alpha1.AtprotoValueFrom) string {
	kind := strings.TrimSpace(from.Kind)
	if kind == "" {
		kind = strings.TrimSpace(from.Resource)
	}
	return kind + " " + strings.TrimSpace(from.Name) + " " + strings.TrimSpace(from.FieldPath)
}

func ReferenceResource(from v1alpha1.AtprotoValueFrom) schema.GroupVersionResource {
	group := v1alpha1.GroupName
	version := v1alpha1.Version
	if raw := strings.TrimSpace(from.ApiVersion); raw != "" {
		if parsed, err := schema.ParseGroupVersion(raw); err == nil {
			group, version = parsed.Group, parsed.Version
		}
	}
	resource := strings.TrimSpace(from.Resource)
	if resource == "" {
		resource = strs.Pluralize(from.Kind)
	}
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

func parseFieldPath(raw string) []fieldStep {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []fieldStep
	for part := range strings.SplitSeq(raw, ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		var indexes []int
		if open := strings.IndexByte(part, '['); open >= 0 {
			name = part[:open]
			rest := part[open:]
			for strings.HasPrefix(rest, "[") {
				close := strings.IndexByte(rest, ']')
				if close < 0 {
					return nil
				}
				index, err := strconv.Atoi(strings.TrimSpace(rest[1:close]))
				if err != nil || index < 0 {
					return nil
				}
				indexes = append(indexes, index)
				rest = rest[close+1:]
			}
			if rest != "" {
				return nil
			}
		}
		if name == "" {
			return nil
		}
		out = append(out, fieldStep{name: name, indexes: indexes})
	}
	return out
}

func fieldValue(object map[string]any, path []fieldStep) (any, bool) {
	var current any = object
	for _, step := range path {
		fields, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = fields[step.name]
		if !ok {
			return nil, false
		}
		for _, index := range step.indexes {
			items, ok := current.([]any)
			if !ok || index >= len(items) {
				return nil, false
			}
			current = items[index]
		}
	}
	return current, true
}

package recordcontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type fieldStep struct {
	name    string
	indexes []int
}

func resolveReferences(ctx context.Context, dyn dynamic.Interface, factory DynamicFactory, workspace string, raw json.RawMessage) (json.RawMessage, []string, error) {
	var value any
	if len(raw) == 0 {
		return nil, []string{"the record is empty"}, nil
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, nil, fmt.Errorf("the record is not valid JSON: %w", err)
	}
	resolved, missing := resolveValue(ctx, dyn, factory, workspace, value)
	if len(missing) > 0 {
		return nil, missing, nil
	}
	out, err := json.Marshal(resolved)
	if err != nil {
		return nil, nil, err
	}
	return out, nil, nil
}

func resolveValue(ctx context.Context, dyn dynamic.Interface, factory DynamicFactory, workspace string, value any) (any, []string) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 1 {
			if raw, ok := typed["valueFrom"]; ok {
				from, err := decodeValueFrom(raw)
				if err != nil {
					return nil, []string{err.Error()}
				}
				return resolveReference(ctx, dyn, factory, workspace, from)
			}
		}
		out := make(map[string]any, len(typed))
		var missing []string
		for key, child := range typed {
			resolved, bad := resolveValue(ctx, dyn, factory, workspace, child)
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
			resolved, bad := resolveValue(ctx, dyn, factory, workspace, child)
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

func resolveReference(ctx context.Context, dyn dynamic.Interface, factory DynamicFactory, workspace string, from v1alpha1.AtprotoValueFrom) (any, []string) {
	label := referenceLabel(from)
	name := strings.TrimSpace(from.Name)
	if name == "" {
		return nil, []string{label + ": the reference names no object"}
	}
	path := parseFieldPath(from.FieldPath)
	if len(path) == 0 {
		return nil, []string{label + ": the reference names no fieldPath"}
	}
	namespace := strings.TrimSpace(from.Namespace)
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	client := dyn
	if target := strings.TrimSpace(from.Workspace); target != "" && target != workspace {
		if factory == nil {
			return nil, []string{label + ": the reference names another workspace and no dynamic factory is configured"}
		}
		var err error
		client, err = factory(target)
		if err != nil {
			return nil, []string{label + ": " + err.Error()}
		}
	}
	if client == nil {
		return nil, []string{label + ": no dynamic client"}
	}
	object, err := client.Resource(gvrFor(from)).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil, []string{label + ": " + err.Error()}
	}
	if err != nil {
		return nil, []string{label + ": " + err.Error()}
	}
	value, ok := fieldValue(object.Object, path)
	if !ok {
		return nil, []string{label + ": the field " + from.FieldPath + " is missing"}
	}
	return value, nil
}

func referenceLabel(from v1alpha1.AtprotoValueFrom) string {
	kind := strings.TrimSpace(from.Kind)
	if kind == "" {
		kind = strings.TrimSpace(from.Resource)
	}
	return kind + " " + strings.TrimSpace(from.Name) + " " + strings.TrimSpace(from.FieldPath)
}

func gvrFor(from v1alpha1.AtprotoValueFrom) schema.GroupVersionResource {
	group := v1alpha1.GroupName
	version := v1alpha1.Version
	if raw := strings.TrimSpace(from.ApiVersion); raw != "" {
		if parsed, err := schema.ParseGroupVersion(raw); err == nil {
			group, version = parsed.Group, parsed.Version
		}
	}
	resource := strings.TrimSpace(from.Resource)
	if resource == "" {
		resource = pluralize(from.Kind)
	}
	return schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
}

func pluralize(kind string) string {
	name := strings.ToLower(strings.TrimSpace(kind))
	switch {
	case name == "":
		return ""
	case strings.HasSuffix(name, "y"):
		return strings.TrimSuffix(name, "y") + "ies"
	case strings.HasSuffix(name, "s"), strings.HasSuffix(name, "x"),
		strings.HasSuffix(name, "ch"), strings.HasSuffix(name, "sh"):
		return name + "es"
	default:
		return name + "s"
	}
}

func parseFieldPath(raw string) []fieldStep {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []fieldStep
	for _, part := range strings.Split(raw, ".") {
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

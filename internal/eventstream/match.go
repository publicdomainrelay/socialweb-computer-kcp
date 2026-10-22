package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

type resolvedMatch struct {
	FieldPath []string

	Value string
}

func (c *Controller) resolveMatches(ctx context.Context, dyn dynamic.Interface, spec recordWatchSpec) ([]resolvedMatch, error) {
	if len(spec.Match) == 0 {
		return nil, nil
	}
	out := make([]resolvedMatch, 0, len(spec.Match))
	for _, match := range spec.Match {
		path := splitFieldPath(match.FieldPath)
		if len(path) == 0 {
			return nil, errors.New("eventstream: a match needs a fieldPath")
		}
		value, err := c.matchValue(ctx, dyn, match)
		if err != nil {
			return nil, err
		}
		out = append(out, resolvedMatch{FieldPath: path, Value: value})
	}
	return out, nil
}

func (c *Controller) matchValue(ctx context.Context, dyn dynamic.Interface, match recordMatch) (string, error) {
	if match.ValueFrom == nil {
		return match.Value, nil
	}
	from := match.ValueFrom
	name := strings.TrimSpace(from.Name)
	if name == "" {
		return "", errors.New("eventstream: a match valueFrom needs a name")
	}
	workspace := strings.TrimSpace(from.Workspace)
	if workspace == "" {
		workspace = c.opts.Workspace
	}
	client := dyn
	if workspace != c.opts.Workspace {
		var err error
		client, err = c.opts.Dynamic(workspace)
		if err != nil {
			return "", err
		}
	}
	gvr := gvrFor(from)
	namespace := strings.TrimSpace(from.Namespace)
	if namespace == "" {
		namespace = metav1.NamespaceDefault
	}
	object, err := client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("eventstream: match valueFrom %s %s/%s does not exist", from.Kind, namespace, name)
	}
	if err != nil {
		return "", err
	}
	path := splitFieldPath(from.FieldPath)
	if len(path) == 0 {
		return "", errors.New("eventstream: a match valueFrom needs a fieldPath")
	}
	return fieldString(object, path)
}

func gvrFor(from *recordValueFrom) schema.GroupVersionResource {
	gv := schema.GroupVersion{Group: "socialweb.computer", Version: "v1alpha1"}
	if raw := strings.TrimSpace(from.ApiVersion); raw != "" {
		if parsed, err := schema.ParseGroupVersion(raw); err == nil {
			gv = parsed
		}
	}
	resource := strings.TrimSpace(from.Resource)
	if resource == "" {
		resource = pluralize(from.Kind)
	}
	return gv.WithResource(resource)
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

func splitFieldPath(path string) []string {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, ".")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func fieldString(object *unstructured.Unstructured, path []string) (string, error) {
	value, ok, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("eventstream: field %s is missing", strings.Join(path, "."))
	}
	return scalarString(value), nil
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case int:
		return strconv.Itoa(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}

func recordFieldString(value any, path []string) (string, bool) {
	current := value
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return "", false
		}
		current, ok = object[part]
		if !ok {
			return "", false
		}
	}
	return scalarString(current), true
}

func matchesRecord(value json.RawMessage, matches []resolvedMatch) bool {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	for _, match := range matches {
		actual, ok := recordFieldString(decoded, match.FieldPath)
		if !ok || actual != match.Value {
			return false
		}
	}
	return true
}

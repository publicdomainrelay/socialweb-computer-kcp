package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type Match struct {
	FieldPath []string

	Value string
}

type ValueFunc func(ctx context.Context, from v1alpha1.AtprotoValueFrom) (map[string]any, error)

func ResolveMatches(ctx context.Context, spec []v1alpha1.AtprotoRecordMatch, value ValueFunc) ([]Match, error) {
	if len(spec) == 0 {
		return nil, nil
	}
	out := make([]Match, 0, len(spec))
	for _, match := range spec {
		path := splitFieldPath(match.FieldPath)
		if len(path) == 0 {
			return nil, errors.New("eventstream: a match needs a fieldPath")
		}
		resolved, err := matchValue(ctx, match, value)
		if err != nil {
			return nil, err
		}
		out = append(out, Match{FieldPath: path, Value: resolved})
	}
	return out, nil
}

func matchValue(ctx context.Context, match v1alpha1.AtprotoRecordMatch, value ValueFunc) (string, error) {
	if match.ValueFrom == nil {
		return match.Value, nil
	}
	from := *match.ValueFrom
	if strings.TrimSpace(from.Name) == "" {
		return "", errors.New("eventstream: a match valueFrom needs a name")
	}
	path := splitFieldPath(from.FieldPath)
	if len(path) == 0 {
		return "", errors.New("eventstream: a match valueFrom needs a fieldPath")
	}
	if value == nil {
		return "", fmt.Errorf("eventstream: match valueFrom %s %s has no reader", from.Kind, from.Name)
	}
	object, err := value(ctx, from)
	if err != nil {
		return "", fmt.Errorf("eventstream: match valueFrom %s %s: %w", from.Kind, from.Name, err)
	}
	resolved, ok := recordFieldString(object, path)
	if !ok {
		return "", fmt.Errorf("eventstream: field %s is missing", strings.Join(path, "."))
	}
	return resolved, nil
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

func MatchesRecord(value json.RawMessage, matches []Match) bool {
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

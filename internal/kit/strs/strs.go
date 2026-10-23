package strs

import (
	"strconv"
	"strings"
)

func AppendUniqueFold(values []string, extra ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(extra))
	out := make([]string, 0, len(values)+len(extra))
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	for _, value := range values {
		add(value)
	}
	for _, value := range extra {
		add(value)
	}
	return out
}

func Pluralize(kind string) string {
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

func AnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func AnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func LastSegment(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "/"); index >= 0 {
		value = value[index+1:]
	}
	return strings.TrimSpace(value)
}

func LeadingInt(v string) (int, error) {
	end := strings.IndexFunc(v, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(v)
	}
	return strconv.Atoi(v[:end])
}

func SplitNUL(b []byte) []string {
	fields := strings.Split(string(b), "\x00")
	if n := len(fields); n > 0 && fields[n-1] == "" {
		return fields[:n-1]
	}
	return fields
}

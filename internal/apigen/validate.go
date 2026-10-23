package apigen

import (
	"cmp"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

type Issue struct {
	Path    string
	Message string
}

func (i Issue) String() string {
	path := i.Path
	path = cmp.Or(path, "(root)")
	return path + ": " + i.Message
}

const (
	msgMissing = "missing required field"
	msgUnknown = "unknown field"
)

func (m *Model) Validate(typeName string, value any) []Issue {
	s, ok := m.Named[typeName]
	if !ok {
		return []Issue{{Message: fmt.Sprintf("no type named %s", typeName)}}
	}
	var out []Issue
	check(s, value, "", &out)
	return out
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func expected(want string, got any) string {
	return fmt.Sprintf("expected %s, got %s", want, describe(got))
}

func check(s *Schema, value any, path string, out *[]Issue) {
	add := func(p, msg string) { *out = append(*out, Issue{Path: p, Message: msg}) }
	switch s.Type {
	case TypeObject:
		obj, ok := value.(map[string]any)
		if !ok {
			add(path, expected("an object", value))
			return
		}
		if s.PreserveUnknown {
			return
		}
		if s.AdditionalProperties != nil {
			keys := make([]string, 0, len(obj))
			for k := range obj {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				check(s.AdditionalProperties, obj[k], joinPath(path, k), out)
			}
			return
		}
		if !s.Closed {
			return
		}
		declared := map[string]bool{}
		for _, p := range s.Properties {
			declared[p.Name] = true
			v, present := obj[p.Name]
			if !present {
				if p.Required {
					add(joinPath(path, p.Name), msgMissing)
				}
				continue
			}
			check(p.Schema, v, joinPath(path, p.Name), out)
		}
		var unknown []string
		for k := range obj {
			if !declared[k] {
				unknown = append(unknown, k)
			}
		}
		sort.Strings(unknown)
		for _, k := range unknown {
			add(joinPath(path, k), msgUnknown)
		}
	case TypeArray:
		arr, ok := value.([]any)
		if !ok {
			add(path, expected("an array", value))
			return
		}
		for i, v := range arr {
			check(s.Items, v, fmt.Sprintf("%s[%d]", path, i), out)
		}
		checkList(s, arr, path, add)
	case TypeString:
		str, ok := value.(string)
		if !ok {
			add(path, expected("a string", value))
			return
		}
		if s.MaxLength != nil && int64(utf8.RuneCountInString(str)) > *s.MaxLength {
			add(path, fmt.Sprintf("%d characters is over the maxLength %d", utf8.RuneCountInString(str), *s.MaxLength))
		}
		if s.Pattern != "" {
			if re, err := regexp.Compile(s.Pattern); err == nil && !re.MatchString(str) {
				add(path, fmt.Sprintf("%q does not match the pattern %s", str, s.Pattern))
			}
		}
		if len(s.Enum) == 0 {
			return
		}
		if slices.Contains(s.Enum, str) {
			return
		}
		add(path, fmt.Sprintf("%q is not one of %s", str, strings.Join(s.Enum, ", ")))
	case TypeInteger:
		n, ok := value.(float64)
		if !ok || n != math.Trunc(n) || math.IsInf(n, 0) {
			add(path, expected("an integer", value))
			return
		}
		if s.Minimum != nil && n < float64(*s.Minimum) {
			add(path, fmt.Sprintf("%v is below the minimum %d", n, *s.Minimum))
		}
		if s.Maximum != nil && n > float64(*s.Maximum) {
			add(path, fmt.Sprintf("%v is above the maximum %d", n, *s.Maximum))
		}
	case TypeBoolean:
		if _, ok := value.(bool); !ok {
			add(path, expected("a boolean", value))
		}
	default:
		add(path, fmt.Sprintf("schema type %q has no validator", s.Type))
	}
}

func checkList(s *Schema, arr []any, path string, add func(p, msg string)) {
	switch s.ListType {
	case ListTypeSet:
		seen := map[any]bool{}
		for i, v := range arr {
			if seen[v] {
				add(fmt.Sprintf("%s[%d]", path, i), "duplicate entry in a set")
			}
			seen[v] = true
		}
	case ListTypeMap:
		seen := map[string]bool{}
		for i, v := range arr {
			obj, ok := v.(map[string]any)
			if !ok {
				continue
			}
			var key strings.Builder
			for _, k := range s.ListMapKeys {
				key.WriteString(fmt.Sprintf("%v\x00", obj[k]))
			}
			if seen[key.String()] {
				add(fmt.Sprintf("%s[%d]", path, i), "duplicate map key "+strings.Join(s.ListMapKeys, ","))
			}
			seen[key.String()] = true
		}
	}
}

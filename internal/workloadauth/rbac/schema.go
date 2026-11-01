package rbac

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"slices"
	"sort"
)

const maxSchemaDepth = 16

type schema struct {
	types []string

	hasConst bool

	constValue any

	enum []any

	items *schema

	required []string

	properties map[string]*schema

	additional *bool

	additionalSchema *schema

	pattern *regexp.Regexp

	minItems *int

	maxItems *int
}

var schemaTypes = []string{"object", "array", "string", "number", "integer", "boolean", "null"}

func compileSchema(raw any) (*schema, error) {
	return compileAt(raw, 0)
}

func compileAt(raw any, depth int) (*schema, error) {
	if depth > maxSchemaDepth {
		return nil, fmt.Errorf("the schema nests deeper than %d", maxSchemaDepth)
	}
	doc, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("a schema is a JSON object")
	}
	s := &schema{}
	for key, value := range doc {
		switch key {
		case "$schema", "description", "title":
		case "type":
			switch v := value.(type) {
			case string:
				s.types = []string{v}
			default:
				list, ok := stringList(v)
				if !ok {
					return nil, fmt.Errorf("type is not a name or a list of names")
				}
				s.types = list
			}
			for _, t := range s.types {
				if !slices.Contains(schemaTypes, t) {
					return nil, fmt.Errorf("type %q is not a JSON type", t)
				}
			}
		case "const":
			s.hasConst, s.constValue = true, value
		case "enum":
			list, ok := value.([]any)
			if !ok || len(list) == 0 {
				return nil, fmt.Errorf("enum is not a non-empty list")
			}
			s.enum = list
		case "items":
			items, err := compileAt(value, depth+1)
			if err != nil {
				return nil, fmt.Errorf("items: %w", err)
			}
			s.items = items
		case "required":
			list, ok := stringList(value)
			if !ok {
				return nil, fmt.Errorf("required is not a list of names")
			}
			s.required = list
		case "properties":
			m, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("properties is not an object")
			}
			s.properties = map[string]*schema{}
			for name, sub := range m {
				compiled, err := compileAt(sub, depth+1)
				if err != nil {
					return nil, fmt.Errorf("properties.%s: %w", name, err)
				}
				s.properties[name] = compiled
			}
		case "additionalProperties":
			switch v := value.(type) {
			case bool:
				s.additional = &v
			default:
				compiled, err := compileAt(v, depth+1)
				if err != nil {
					return nil, fmt.Errorf("additionalProperties: %w", err)
				}
				s.additionalSchema = compiled
			}
		case "pattern":
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("pattern is not a string")
			}
			re, err := regexp.Compile(text)
			if err != nil {
				return nil, fmt.Errorf("pattern: %w", err)
			}
			s.pattern = re
		case "minItems", "maxItems":
			n, ok := value.(float64)
			if !ok || n < 0 || n != math.Trunc(n) {
				return nil, fmt.Errorf("%s is not a count", key)
			}
			count := int(n)
			if key == "minItems" {
				s.minItems = &count
			} else {
				s.maxItems = &count
			}
		default:
			return nil, fmt.Errorf("keyword %q is not in the supported subset", key)
		}
	}
	return s, nil
}

func normalize(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("the body is not JSON: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the body is not JSON: %w", err)
	}
	return out, nil
}

func typeOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if t == math.Trunc(t) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func (s *schema) validate(v any, at string) error {
	if len(s.types) > 0 {
		got := typeOf(v)
		ok := slices.Contains(s.types, got) || (got == "integer" && slices.Contains(s.types, "number"))
		if !ok {
			return fmt.Errorf("%s is %s, not %v", at, got, s.types)
		}
	}
	if s.hasConst && !reflect.DeepEqual(v, s.constValue) {
		return fmt.Errorf("%s is not the allowed value", at)
	}
	if len(s.enum) > 0 && !slices.ContainsFunc(s.enum, func(e any) bool { return reflect.DeepEqual(v, e) }) {
		return fmt.Errorf("%s is not one of the allowed values", at)
	}
	if s.pattern != nil {
		text, ok := v.(string)
		if !ok || !s.pattern.MatchString(text) {
			return fmt.Errorf("%s does not match the allowed pattern", at)
		}
	}
	if list, ok := v.([]any); ok {
		if s.minItems != nil && len(list) < *s.minItems {
			return fmt.Errorf("%s has fewer than %d items", at, *s.minItems)
		}
		if s.maxItems != nil && len(list) > *s.maxItems {
			return fmt.Errorf("%s has more than %d items", at, *s.maxItems)
		}
		if s.items != nil {
			for i, item := range list {
				if err := s.items.validate(item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, name := range s.required {
			if _, ok := obj[name]; !ok {
				return fmt.Errorf("%s.%s is required", at, name)
			}
		}
		names := make([]string, 0, len(obj))
		for name := range obj {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value := obj[name]
			if sub, ok := s.properties[name]; ok {
				if err := sub.validate(value, at+"."+name); err != nil {
					return err
				}
				continue
			}
			if s.additional != nil && !*s.additional {
				return fmt.Errorf("%s.%s is not allowed", at, name)
			}
			if s.additionalSchema != nil {
				if err := s.additionalSchema.validate(value, at+"."+name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

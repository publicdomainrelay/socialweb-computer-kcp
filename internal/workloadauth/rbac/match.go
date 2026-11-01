package rbac

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/workloadauth"
)

var methodCapability = map[string]string{
	"GET":     workloadauth.CapabilityRead,
	"HEAD":    workloadauth.CapabilityRead,
	"OPTIONS": workloadauth.CapabilityRead,
	"POST":    workloadauth.CapabilityCreate,
	"PUT":     workloadauth.CapabilityUpdate,
	"PATCH":   workloadauth.CapabilityUpdate,
	"DELETE":  workloadauth.CapabilityDelete,
}

var knownCapabilities = []string{
	workloadauth.CapabilityRead, workloadauth.CapabilityCreate, workloadauth.CapabilityUpdate, workloadauth.CapabilityDelete,
}

type pathSchema struct {
	capabilities []string

	body *schema

	bodyRequired bool
}

func parsePathSchema(raw json.RawMessage) (pathSchema, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return pathSchema{}, fmt.Errorf("the path schema is not a JSON object: %w", err)
	}
	out := pathSchema{}
	var properties map[string]any
	for key, value := range doc {
		switch key {
		case "$schema", "description", "title":
		case "type":
			if value != "object" {
				return pathSchema{}, fmt.Errorf("the path schema type is %v, not object", value)
			}
		case "required":
			names, ok := stringList(value)
			if !ok {
				return pathSchema{}, fmt.Errorf("required is not a list of names")
			}
			for _, name := range names {
				switch name {
				case "capability":
				case "body":
					out.bodyRequired = true
				default:
					return pathSchema{}, fmt.Errorf("required names %q; a path schema has capability and body only", name)
				}
			}
		case "properties":
			m, ok := value.(map[string]any)
			if !ok {
				return pathSchema{}, fmt.Errorf("properties is not an object")
			}
			properties = m
		default:
			return pathSchema{}, fmt.Errorf("keyword %q is not one a path schema may carry", key)
		}
	}
	for key, value := range properties {
		switch key {
		case "capability":
			m, ok := value.(map[string]any)
			if !ok {
				return pathSchema{}, fmt.Errorf("capability is not an object")
			}
			for k := range m {
				if k != "enum" && k != "type" {
					return pathSchema{}, fmt.Errorf("capability carries %q; it takes an enum only", k)
				}
			}
			caps, ok := stringList(m["enum"])
			if !ok || len(caps) == 0 {
				return pathSchema{}, fmt.Errorf("capability.enum is not a list of capabilities")
			}
			for _, c := range caps {
				if !slices.Contains(knownCapabilities, c) {
					return pathSchema{}, fmt.Errorf("capability %q is not one of %v", c, knownCapabilities)
				}
			}
			out.capabilities = caps
		case "body":
			body, err := compileSchema(value)
			if err != nil {
				return pathSchema{}, fmt.Errorf("body: %w", err)
			}
			out.body = body
		default:
			return pathSchema{}, fmt.Errorf("property %q is not one a path schema may carry", key)
		}
	}
	if len(out.capabilities) == 0 {
		return pathSchema{}, fmt.Errorf("the path schema names no capability")
	}
	return out, nil
}

func stringList(value any) ([]string, bool) {
	list, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func CapabilityOf(method string) (string, bool) {
	c, ok := methodCapability[strings.ToUpper(method)]
	return c, ok
}

func matchPath(schemas map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	if raw, ok := schemas[path]; ok {
		return raw, true
	}
	best := ""
	var found json.RawMessage
	for pattern, raw := range schemas {
		if len(pattern) > len(best) && globMatch(pattern, path) {
			best, found = pattern, raw
		}
	}
	return found, best != ""
}

func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return len(s) >= len(last) && strings.HasSuffix(s, last)
}

func (r Record) Allows(action workloadauth.Action) error {
	capability, ok := CapabilityOf(action.Method)
	if !ok {
		return fmt.Errorf("%w: method %s maps to no capability", workloadauth.ErrDenied, action.Method)
	}
	name, role := r.Role()
	var denials []string
	for _, policyName := range role.Definition.Policies {
		raw, ok := matchPath(r.Policies[policyName].Schemas, action.Path)
		if !ok {
			continue
		}
		schema, err := parsePathSchema(raw)
		if err != nil {
			denials = append(denials, fmt.Sprintf("policy %q: %v", policyName, err))
			continue
		}
		if !slices.Contains(schema.capabilities, capability) {
			denials = append(denials, fmt.Sprintf("policy %q: capability %s is not in %v for %s", policyName, capability, schema.capabilities, action.Path))
			continue
		}
		if schema.bodyRequired && action.Body == nil {
			denials = append(denials, fmt.Sprintf("policy %q: %s needs a body", policyName, action.Path))
			continue
		}
		if schema.body != nil {
			body, err := normalize(action.Body)
			if err != nil {
				denials = append(denials, fmt.Sprintf("policy %q: %v", policyName, err))
				continue
			}
			if err := schema.body.validate(body, "body"); err != nil {
				denials = append(denials, fmt.Sprintf("policy %q: %v", policyName, err))
				continue
			}
		}
		return nil
	}
	if len(denials) > 0 {
		return fmt.Errorf("%w: %s", workloadauth.ErrDenied, strings.Join(denials, "; "))
	}
	return fmt.Errorf("%w: no policy of role %q covers %s", workloadauth.ErrDenied, name, action.Path)
}

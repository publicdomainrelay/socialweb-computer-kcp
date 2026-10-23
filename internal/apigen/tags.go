package apigen

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const (
	userEndpointTag = "userEndpoint"

	keyDefault    = "default"
	keyMinimum    = "minimum"
	keyMaximum    = "maximum"
	keyMaxLength  = "maxLength"
	keyPattern    = "pattern"
	keyListType   = "listType"
	keyListMapKey = "listMapKey"
	keyEnum       = "enum"

	listValueSeparator = "|"

	ListTypeAtomic = "atomic"
	ListTypeSet    = "set"
	ListTypeMap    = "map"
)

type fieldTag struct {
	Optional     bool
	UserEndpoint bool
	Default      *string
	Minimum      *int64
	Maximum      *int64
	MaxLength    *int64
	Pattern      string
	ListType     string
	ListMapKeys  []string
	Enum         []string
}

func (t fieldTag) keywords() bool {
	return t.Default != nil || t.Minimum != nil || t.Maximum != nil || t.MaxLength != nil || t.Pattern != "" ||
		t.ListType != "" || len(t.ListMapKeys) > 0 || len(t.Enum) > 0
}

func parseTag(raw string) (fieldTag, error) {
	var t fieldTag
	rest := raw
	for rest != "" {
		var opt string
		if strings.HasPrefix(rest, keyPattern+"=") {
			opt, rest = rest, ""
		} else {
			opt, rest, _ = strings.Cut(rest, ",")
		}
		if opt == "" {
			continue
		}
		key, value, hasValue := strings.Cut(opt, "=")
		if !hasValue {
			switch key {
			case optionalTag:
				t.Optional = true
			case userEndpointTag:
				t.UserEndpoint = true
			default:
				return t, fmt.Errorf("unknown apigen tag option %q", opt)
			}
			continue
		}
		var err error
		switch key {
		case keyDefault:
			v := value
			t.Default = &v
		case keyMinimum:
			t.Minimum, err = parseInt(key, value)
		case keyMaximum:
			t.Maximum, err = parseInt(key, value)
		case keyMaxLength:
			t.MaxLength, err = parseInt(key, value)
			if err == nil && *t.MaxLength < 0 {
				err = fmt.Errorf("maxLength %d is negative", *t.MaxLength)
			}
		case keyPattern:
			if _, cerr := regexp.Compile(value); cerr != nil {
				err = fmt.Errorf("pattern %q does not compile: %w", value, cerr)
			}
			t.Pattern = value
		case keyListType:
			switch value {
			case ListTypeAtomic, ListTypeSet, ListTypeMap:
				t.ListType = value
			default:
				err = fmt.Errorf("listType %q is not one of atomic, set, map", value)
			}
		case keyListMapKey:
			t.ListMapKeys = splitValues(value)
		case keyEnum:
			t.Enum = splitValues(value)
		default:
			err = fmt.Errorf("unknown apigen tag option %q", opt)
		}
		if err != nil {
			return t, err
		}
	}
	if t.Minimum != nil && t.Maximum != nil && *t.Minimum > *t.Maximum {
		return t, fmt.Errorf("minimum %d is above maximum %d", *t.Minimum, *t.Maximum)
	}
	if len(t.ListMapKeys) > 0 && t.ListType != ListTypeMap {
		return t, fmt.Errorf("listMapKey needs listType=map")
	}
	if t.ListType == ListTypeMap && len(t.ListMapKeys) == 0 {
		return t, fmt.Errorf("listType=map needs listMapKey")
	}
	return t, nil
}

func parseInt(key, value string) (*int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not an integer", key, value)
	}
	return &n, nil
}

func splitValues(value string) []string {
	var out []string
	for v := range strings.SplitSeq(value, listValueSeparator) {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (t fieldTag) apply(s *Schema) (*Schema, error) {
	if !t.keywords() {
		return s, nil
	}
	c := *s
	if len(t.Enum) > 0 {
		if c.Type != TypeString {
			return nil, fmt.Errorf("enum applies to a string, the field is %s", c.Type)
		}
		if len(c.Enum) > 0 {
			for _, v := range t.Enum {
				if !contains(c.Enum, v) {
					return nil, fmt.Errorf("enum value %q is not a constant of %s", v, c.Ref)
				}
			}
		}
		c.Enum = t.Enum
	}
	if t.Minimum != nil || t.Maximum != nil {
		if c.Type != TypeInteger {
			return nil, fmt.Errorf("minimum and maximum apply to an integer, the field is %s", c.Type)
		}
		c.Minimum, c.Maximum = t.Minimum, t.Maximum
	}
	if t.MaxLength != nil || t.Pattern != "" {
		if c.Type != TypeString {
			return nil, fmt.Errorf("maxLength and pattern apply to a string, the field is %s", c.Type)
		}
		c.MaxLength, c.Pattern = t.MaxLength, t.Pattern
	}
	if t.ListType != "" {
		if c.Type != TypeArray {
			return nil, fmt.Errorf("listType applies to an array, the field is %s", c.Type)
		}
		c.ListType = t.ListType
		if t.ListType == ListTypeSet && c.Items.Type != TypeString && c.Items.Type != TypeInteger {
			return nil, fmt.Errorf("listType=set needs scalar items, the items are %s", c.Items.Type)
		}
		if t.ListType == ListTypeMap {
			if c.Items.Type != TypeObject || len(c.Items.Properties) == 0 {
				return nil, fmt.Errorf("listType=map needs object items")
			}
			for _, k := range t.ListMapKeys {
				p := c.Items.property(k)
				if p == nil {
					return nil, fmt.Errorf("listMapKey %q names no field of the items", k)
				}
				if !p.Required {
					return nil, fmt.Errorf("listMapKey %q names a field that is not required", k)
				}
			}
			c.ListMapKeys = t.ListMapKeys
		}
	}
	if t.Default != nil {
		v, err := defaultValue(&c, *t.Default)
		if err != nil {
			return nil, err
		}
		c.Default = v
		var issues []Issue
		check(&c, v, "", &issues)
		if len(issues) > 0 {
			return nil, fmt.Errorf("default %q does not validate: %s", *t.Default, issues[0])
		}
	}
	return &c, nil
}

func (s *Schema) property(name string) *Property {
	for i := range s.Properties {
		if s.Properties[i].Name == name {
			return &s.Properties[i]
		}
	}
	return nil
}

func contains(values []string, v string) bool {
	return slices.Contains(values, v)
}

func defaultValue(s *Schema, raw string) (any, error) {
	switch s.Type {
	case TypeString:
		return raw, nil
	case TypeInteger:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("default %q is not an integer", raw)
		}
		return float64(n), nil
	case TypeBoolean:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("default %q is not a boolean", raw)
		}
		return b, nil
	default:
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("default %q for a %s is not JSON: %w", raw, s.Type, err)
		}
		return v, nil
	}
}

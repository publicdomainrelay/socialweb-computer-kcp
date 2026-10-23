package plc

import (
	"encoding/json"
	"fmt"
	"maps"
)

const (
	TypeOperation = "plc_operation"
	TypeTombstone = "plc_tombstone"
	TypeCreate    = "create"
)

type Service struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
}

type Operation struct {
	Type                string
	Prev                *string
	RotationKeys        []string
	VerificationMethods map[string]string
	AlsoKnownAs         []string
	Services            map[string]Service
	SigningKey          string
	RecoveryKey         string
	Handle              string
	Service             string
	Sig                 string

	raw map[string]any
}

type LogEntry struct {
	DID       string    `json:"did"`
	Operation Operation `json:"operation"`
	CID       string    `json:"cid"`
	Nullified bool      `json:"nullified"`
	CreatedAt string    `json:"createdAt"`
}

type VerificationMethod struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Controller         string `json:"controller"`
	PublicKeyMultibase string `json:"publicKeyMultibase"`
}

type DIDService struct {
	ID              string `json:"id"`
	Type            string `json:"type"`
	ServiceEndpoint string `json:"serviceEndpoint"`
}

type DIDDocument struct {
	Context            []string             `json:"@context"`
	ID                 string               `json:"id"`
	AlsoKnownAs        []string             `json:"alsoKnownAs,omitempty"`
	VerificationMethod []VerificationMethod `json:"verificationMethod,omitempty"`
	Service            []DIDService         `json:"service,omitempty"`
}

func (o Operation) MarshalJSON() ([]byte, error) {
	if o.raw != nil {
		return json.Marshal(o.raw)
	}
	return json.Marshal(o.object())
}

func (o *Operation) UnmarshalJSON(b []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	o.fromObject(raw)
	o.raw = raw
	return nil
}

func NewOperationFromObject(raw map[string]any) *Operation {
	op := &Operation{}
	op.fromObject(raw)
	op.raw = raw
	return op
}

func (o *Operation) SignedObject() map[string]any {
	m := o.object()
	if o.Sig != "" {
		m["sig"] = o.Sig
	}
	return m
}

func (o *Operation) UnsignedObject() map[string]any {
	m := o.object()
	delete(m, "sig")
	return m
}

func (o *Operation) object() map[string]any {
	if o.raw != nil {
		out := make(map[string]any, len(o.raw))
		maps.Copy(out, o.raw)
		return out
	}
	m := map[string]any{"type": o.Type}
	switch o.Type {
	case TypeOperation:
		m["rotationKeys"] = stringsAsAny(o.RotationKeys)
		m["verificationMethods"] = mapAsAny(o.VerificationMethods)
		m["alsoKnownAs"] = stringsAsAny(o.AlsoKnownAs)
		services := make(map[string]any, len(o.Services))
		for k, v := range o.Services {
			services[k] = map[string]any{"type": v.Type, "endpoint": v.Endpoint}
		}
		m["services"] = services
		if o.Prev != nil {
			m["prev"] = *o.Prev
		} else {
			m["prev"] = nil
		}
	case TypeTombstone:
		if o.Prev != nil {
			m["prev"] = *o.Prev
		} else {
			m["prev"] = nil
		}
	case TypeCreate:
		m["signingKey"] = o.SigningKey
		m["recoveryKey"] = o.RecoveryKey
		m["handle"] = o.Handle
		m["service"] = o.Service
	}
	if o.Sig != "" {
		m["sig"] = o.Sig
	}
	return m
}

func (o *Operation) fromObject(raw map[string]any) {
	o.raw = nil
	o.Type, _ = raw["type"].(string)
	o.Sig, _ = raw["sig"].(string)
	o.Prev = nil
	if v, ok := raw["prev"]; ok && v != nil {
		if s, ok := v.(string); ok {
			o.Prev = &s
		}
	}
	o.RotationKeys = stringsFromAny(raw["rotationKeys"])
	o.VerificationMethods = stringMapFromAny(raw["verificationMethods"])
	o.AlsoKnownAs = stringsFromAny(raw["alsoKnownAs"])
	o.Services = servicesFromAny(raw["services"])
	o.SigningKey, _ = raw["signingKey"].(string)
	o.RecoveryKey, _ = raw["recoveryKey"].(string)
	o.Handle, _ = raw["handle"].(string)
	o.Service, _ = raw["service"].(string)
}

func stringsAsAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func mapAsAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func stringsFromAny(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, el := range arr {
		if s, ok := el.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMapFromAny(v any) map[string]string {
	m, _ := v.(map[string]any)
	out := make(map[string]string, len(m))
	for k, el := range m {
		if s, ok := el.(string); ok {
			out[k] = s
		}
	}
	return out
}

func servicesFromAny(v any) map[string]Service {
	m, _ := v.(map[string]any)
	out := make(map[string]Service, len(m))
	for k, el := range m {
		svc, ok := el.(map[string]any)
		if !ok {
			continue
		}
		s := Service{}
		s.Type, _ = svc["type"].(string)
		s.Endpoint, _ = svc["endpoint"].(string)
		out[k] = s
	}
	return out
}

func ValidateStructure(raw map[string]any) string {
	if raw == nil {
		return "Operation must be an object"
	}
	typ, _ := raw["type"].(string)
	switch typ {
	case TypeOperation:
		if _, ok := raw["rotationKeys"].([]any); !ok {
			return "rotationKeys must be an array"
		}
		if v, ok := raw["verificationMethods"]; ok && v != nil {
			if _, ok := v.(map[string]any); !ok {
				return "verificationMethods must be an object"
			}
		}
		if v, ok := raw["alsoKnownAs"]; ok && v != nil {
			if _, ok := v.([]any); !ok {
				return "alsoKnownAs must be an array"
			}
		}
		if v, ok := raw["services"]; ok && v != nil {
			if _, ok := v.(map[string]any); !ok {
				return "services must be an object"
			}
		}
		if _, ok := raw["sig"].(string); !ok {
			return "sig must be a string"
		}
		if v, ok := raw["prev"]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return "prev must be null or a CID string"
			}
		}
		return ""
	case TypeTombstone:
		if _, ok := raw["prev"].(string); !ok {
			return "prev must be a CID string"
		}
		if _, ok := raw["sig"].(string); !ok {
			return "sig must be a string"
		}
		return ""
	case TypeCreate:
		if _, ok := raw["signingKey"].(string); !ok {
			return "signingKey must be a string"
		}
		if _, ok := raw["recoveryKey"].(string); !ok {
			return "recoveryKey must be a string"
		}
		if _, ok := raw["handle"].(string); !ok {
			return "handle must be a string"
		}
		if _, ok := raw["service"].(string); !ok {
			return "service must be a string"
		}
		if _, ok := raw["sig"].(string); !ok {
			return "sig must be a string"
		}
		return ""
	default:
		if raw["type"] == nil {
			return "Unknown operation type: undefined"
		}
		return fmt.Sprintf("Unknown operation type: %v", raw["type"])
	}
}

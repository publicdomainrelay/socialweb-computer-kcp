package b64

import (
	"encoding/base64"
	"encoding/json"
)

func Loose(s string) ([]byte, bool) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

func NormalizeJSON(value any) any {
	switch v := value.(type) {
	case map[string]any:
		if s, ok := v["$bytes"].(string); ok && len(v) == 1 {
			if b, ok := Loose(s); ok {
				return map[string]any{"$bytes": base64.RawStdEncoding.EncodeToString(b)}
			}
			return v
		}
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = NormalizeJSON(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = NormalizeJSON(item)
		}
		return out
	default:
		return value
	}
}

func NormalizeJSONBytes(raw []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(NormalizeJSON(value))
}

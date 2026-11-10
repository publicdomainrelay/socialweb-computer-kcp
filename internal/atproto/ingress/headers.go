package ingress

import (
	"encoding/json"
	"net/http"
	"strings"
)

type HeaderValues []string

func (h *HeaderValues) UnmarshalJSON(b []byte) error {
	var one string
	if json.Unmarshal(b, &one) == nil {
		*h = HeaderValues{one}
		return nil
	}
	var many []string
	if json.Unmarshal(b, &many) == nil {
		*h = many
		return nil
	}
	*h = nil
	return nil
}

func joinedHeaders(in map[string]HeaderValues) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

const (
	maxResponseHeaderValues = 64
	maxResponseHeaderValue  = 8 << 10
	maxResponseHeaderBytes  = 32 << 10
)

var relayOwnedResponseHeaders = map[string]struct{}{
	"connection": {}, "keep-alive": {}, "transfer-encoding": {}, "upgrade": {}, "te": {}, "trailer": {},
	"content-length": {}, "strict-transport-security": {}, "alt-svc": {},
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0 {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 && c != '\t' || c == 0x7f {
			return false
		}
	}
	return true
}

func hostOnlyCookie(value string) bool {
	_, attrs, _ := strings.Cut(value, ";")
	for attr := range strings.SplitSeq(attrs, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(attr), "=")
		if strings.EqualFold(strings.TrimSpace(name), "domain") {
			return false
		}
	}
	return true
}

func responseHeaders(in map[string]HeaderValues, contentType string) (http.Header, []string) {
	out := http.Header{}
	dropped := []string{}
	count, size := 0, 0
	for name, values := range in {
		lower := strings.ToLower(name)
		_, owned := relayOwnedResponseHeaders[lower]
		if owned || strings.HasPrefix(lower, "proxy-") || !validHeaderName(name) || (lower == "content-type" && contentType != "") {
			dropped = append(dropped, lower)
			continue
		}
		for _, value := range values {
			if !validHeaderValue(value) || len(value) > maxResponseHeaderValue || (lower == "set-cookie" && !hostOnlyCookie(value)) {
				dropped = append(dropped, lower)
				continue
			}
			if count++; count > maxResponseHeaderValues || size+len(name)+len(value) > maxResponseHeaderBytes {
				dropped = append(dropped, lower)
				continue
			}
			size += len(name) + len(value)
			out.Add(name, value)
		}
	}
	return out, dropped
}

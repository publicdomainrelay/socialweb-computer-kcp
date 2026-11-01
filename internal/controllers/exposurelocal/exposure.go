package exposurelocal

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const ConditionReady = "Ready"

type Origin struct {
	URL string

	Host string
}

func (o Origin) String() string { return o.URL }

func ParseOrigin(raw string) (Origin, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Origin{}, errors.New("exposurelocal: an origin is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q is not http or https", raw)
	}
	if u.User != nil {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q has credentials", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q has a path", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q has a query or fragment", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return Origin{}, fmt.Errorf("exposurelocal: the origin %q has no host", raw)
	}
	if u.Scheme == "http" && !loopback(host) {
		return Origin{}, fmt.Errorf("exposurelocal: http is only allowed for localhost, not %q", host)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Origin{}, fmt.Errorf("exposurelocal: the origin %q has an invalid port", raw)
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return Origin{URL: u.Scheme + "://" + authority, Host: authority}, nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func Status(generation int64, origin, value string) exposurev1alpha1.ExposureStatus {
	address := Address(origin, value)
	st := exposurev1alpha1.ExposureStatus{
		Phase:    exposurev1alpha1.ExposureReady,
		Ready:    true,
		Address:  address,
		Endpoint: address,
	}
	conds.Set(&st.Conditions, generation, ConditionReady, true, "Ready", "the in-process Exposure serves "+address)
	return st
}

func NotReady(generation int64, phase exposurev1alpha1.ExposurePhase, reason, message string) exposurev1alpha1.ExposureStatus {
	st := exposurev1alpha1.ExposureStatus{Phase: phase}
	conds.Set(&st.Conditions, generation, ConditionReady, false, reason, message)
	return st
}

func Address(origin, value string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if p := normalizePath(value); p != "/" {
		return origin + p
	}
	return origin
}

func normalizePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if value = path.Clean(value); value == "." {
		return "/"
	}
	return value
}

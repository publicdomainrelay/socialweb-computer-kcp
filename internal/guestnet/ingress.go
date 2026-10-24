package guestnet

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	EventsPath = "/v1/events"

	RelayHost = "relay.localhost"
)

var (
	bundleOnNetworkURL = regexp.MustCompile(`"guest_onnetwork_url"\s*:\s*"([^"]+)"`)

	relayAddress = regexp.MustCompile(`(\d+\.\d+\.\d+\.\d+)\s+relay\.localhost`)
)

type RelayTarget struct {
	URL string

	Resolve string
}

func GuestOnNetworkURL(ingressURL string) (string, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(ingressURL), "/")
	if trimmed == "" {
		return "", fmt.Errorf("guestnet: no ingress URL")
	}
	u, err := url.Parse(trimmed)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("guestnet: ingress URL %q is not an absolute http or https URL", ingressURL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("guestnet: ingress URL %q carries a user, query or fragment", ingressURL)
	}
	return trimmed + EventsPath, nil
}

func OnNetworkURLFromBundle(userData, fallback string) string {
	if m := bundleOnNetworkURL.FindStringSubmatch(userData); m != nil {
		return m[1]
	}
	url, err := GuestOnNetworkURL(fallback)
	if err != nil {
		return ""
	}
	return url
}

func RelayTargets(userData, fallback string) []RelayTarget {
	target := OnNetworkURLFromBundle(userData, fallback)
	targets := []RelayTarget{{URL: target}}
	address, ok := RequesterRelayAddress(userData)
	if !ok || target == "" {
		return targets
	}
	u, err := url.Parse(target)
	if err != nil || !strings.HasSuffix(u.Hostname(), "."+RelayHost) {
		return targets
	}
	port := u.Port()
	port = cmp.Or(port, "80")
	targets = append(targets, RelayTarget{URL: target, Resolve: u.Hostname() + ":" + port + ":" + address})
	return targets
}

func RequesterRelayAddress(userData string) (string, bool) {
	m := relayAddress.FindStringSubmatch(userData)
	if m == nil {
		return "", false
	}
	return m[1], true
}

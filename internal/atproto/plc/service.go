package plc

import (
	"cmp"
	"fmt"
	"net/url"
	"strings"
)

const PDSServiceID = "atproto_pds"

const PDSServiceType = "AtprotoPersonalDataServer"

func NormalizeServiceEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("plc: a service endpoint is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("plc: invalid service endpoint %q: %w", raw, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("plc: service endpoint %q must use https", raw)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("plc: service endpoint %q must not have userinfo", raw)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("plc: service endpoint %q must have a host", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("plc: service endpoint %q must not have a path", raw)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("plc: service endpoint %q must not have a query or fragment", raw)
	}
	return "https://" + parsed.Host, nil
}

func CreateServiceUpdate(prev *Operation, prevCID string, endpoint string) (*Operation, error) {
	if prev == nil {
		return nil, fmt.Errorf("plc: a service update needs the previous operation")
	}
	if prev.Type != TypeOperation {
		return nil, fmt.Errorf("plc: a service update needs a plc_operation, got %q", prev.Type)
	}
	if prevCID == "" {
		return nil, fmt.Errorf("plc: a service update needs the previous operation cid")
	}
	normalized, err := NormalizeServiceEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	services := copyServiceMap(prev.Services)
	service := services[PDSServiceID]
	service.Type = cmp.Or(service.Type, PDSServiceType)
	service.Endpoint = normalized
	services[PDSServiceID] = service
	previous := prevCID
	return &Operation{
		Type:                TypeOperation,
		Prev:                &previous,
		RotationKeys:        append([]string{}, prev.RotationKeys...),
		VerificationMethods: copyStringMap(prev.VerificationMethods),
		AlsoKnownAs:         append([]string{}, prev.AlsoKnownAs...),
		Services:            services,
	}, nil
}

func VerifyServiceOperation(op *Operation, prev *Operation, endpoint string) error {
	if op == nil {
		return fmt.Errorf("plc: missing service update operation")
	}
	if prev == nil {
		return fmt.Errorf("plc: missing previous operation")
	}
	normalized, err := NormalizeServiceEndpoint(endpoint)
	if err != nil {
		return err
	}
	if op.Type != TypeOperation {
		return fmt.Errorf("plc: service update operation type is %q", op.Type)
	}
	if !sameStrings(op.RotationKeys, prev.RotationKeys) {
		return fmt.Errorf("plc: service update did not preserve rotation keys")
	}
	if !sameStringMap(op.VerificationMethods, prev.VerificationMethods) {
		return fmt.Errorf("plc: service update did not preserve verification methods")
	}
	if !sameStrings(op.AlsoKnownAs, prev.AlsoKnownAs) {
		return fmt.Errorf("plc: service update did not preserve alsoKnownAs")
	}
	for key, service := range prev.Services {
		if key == PDSServiceID {
			continue
		}
		if op.Services[key] != service {
			return fmt.Errorf("plc: service update did not preserve service %q", key)
		}
	}
	service, ok := op.Services[PDSServiceID]
	if !ok {
		return fmt.Errorf("plc: service update is missing %q", PDSServiceID)
	}
	if service.Type != PDSServiceType {
		if prevService, ok := prev.Services[PDSServiceID]; !ok || prevService.Type != service.Type {
			return fmt.Errorf("plc: service update has an unexpected %q type", PDSServiceID)
		}
	}
	if service.Endpoint != normalized {
		return fmt.Errorf("plc: service update endpoint is %q, want %q", service.Endpoint, normalized)
	}
	return nil
}

func VerifyServiceDocument(doc *DIDDocument, did string, endpoint string) error {
	if doc == nil {
		return fmt.Errorf("plc: the DID document for %s is missing", did)
	}
	if doc.ID != did {
		return fmt.Errorf("plc: the DID document id is %q, want %q", doc.ID, did)
	}
	found := false
	for _, service := range doc.Service {
		if service.ID != "#"+PDSServiceID && !strings.HasSuffix(service.ID, "#"+PDSServiceID) {
			continue
		}
		found = true
		if service.Type != PDSServiceType {
			return fmt.Errorf("plc: the DID document service type is %q, want %q", service.Type, PDSServiceType)
		}
		if service.ServiceEndpoint != endpoint {
			return fmt.Errorf("plc: the DID document endpoint is %q, want %q", service.ServiceEndpoint, endpoint)
		}
	}
	if !found {
		return fmt.Errorf("plc: the DID document for %s has no %q service", did, PDSServiceID)
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

package plc

import (
	"maps"
	"sort"
	"strings"
)

var didContext = []string{
	"https://www.w3.org/ns/did/v1",
	"https://w3id.org/security/multikey/v1",
}

func ResolveDocument(did string, current []LogEntry) *DIDDocument {
	if len(current) == 0 {
		return nil
	}
	if current[len(current)-1].Operation.Type == TypeTombstone {
		return nil
	}

	verificationMethods := map[string]string{}
	alsoKnownAs := []string{}
	services := map[string]Service{}

	switch op := current[len(current)-1].Operation; op.Type {
	case TypeOperation:
		maps.Copy(verificationMethods, op.VerificationMethods)
		alsoKnownAs = dedupe(op.AlsoKnownAs)
		maps.Copy(services, op.Services)
	case TypeCreate:
		verificationMethods["atproto"] = op.SigningKey
		if op.Handle != "" {
			alsoKnownAs = []string{op.Handle}
		}
		services["atproto_pds"] = Service{Type: "AtprotoPersonalDataServer", Endpoint: op.Service}
	}

	doc := &DIDDocument{Context: append([]string{}, didContext...), ID: did}
	if len(alsoKnownAs) > 0 {
		doc.AlsoKnownAs = alsoKnownAs
	}

	vmKeys := make([]string, 0, len(verificationMethods))
	for k := range verificationMethods {
		vmKeys = append(vmKeys, k)
	}
	sort.Strings(vmKeys)
	for _, id := range vmKeys {
		key := verificationMethods[id]
		key = strings.TrimPrefix(key, "did:key:")
		doc.VerificationMethod = append(doc.VerificationMethod, VerificationMethod{
			ID:                 "#" + id,
			Type:               "Multikey",
			Controller:         did,
			PublicKeyMultibase: key,
		})
	}

	svcKeys := make([]string, 0, len(services))
	for k := range services {
		svcKeys = append(svcKeys, k)
	}
	sort.Strings(svcKeys)
	for _, id := range svcKeys {
		s := services[id]
		doc.Service = append(doc.Service, DIDService{
			ID:              "#" + id,
			Type:            s.Type,
			ServiceEndpoint: s.Endpoint,
		})
	}

	return doc
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

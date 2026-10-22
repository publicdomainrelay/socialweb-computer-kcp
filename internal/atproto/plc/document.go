package plc

import (
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

	for _, entry := range current {
		op := entry.Operation
		switch op.Type {
		case TypeOperation:
			for k, v := range op.VerificationMethods {
				verificationMethods[k] = v
			}
			alsoKnownAs = dedupe(append(alsoKnownAs, op.AlsoKnownAs...))
			for k, v := range op.Services {
				services[k] = v
			}
		case TypeCreate:
			next := map[string]string{"atproto": op.SigningKey}
			for k, v := range verificationMethods {
				next[k] = v
			}
			verificationMethods = next
			if op.Handle != "" {
				alsoKnownAs = dedupe(append(alsoKnownAs, op.Handle))
			}
			nextServices := map[string]Service{
				"atproto_pds": {Type: "AtprotoPersonalDataServer", Endpoint: op.Service},
			}
			for k, v := range services {
				nextServices[k] = v
			}
			services = nextServices
		}
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

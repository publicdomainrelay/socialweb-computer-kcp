package trust

import (
	"slices"
	"strings"
)

const (
	KindClaim       = "claim"
	KindAssociation = "association"
	KindBskyPost    = "bskyPost"
	KindDelvePost   = "delvePost"

	DefaultPostTemplate = "associate {did}"
)

var DefaultKinds = []string{KindClaim, KindAssociation}

type Association struct {
	Kinds           []string
	PostTemplate    string
	MaxDepth        int
	RequireContract bool
}

type associationKey struct {
	claim           bool
	association     bool
	bskyPost        bool
	delvePost       bool
	template        string
	requireContract bool
}

func (a Association) key() associationKey {
	kinds := a.Kinds
	if len(kinds) == 0 {
		kinds = DefaultKinds
	}
	template := a.PostTemplate
	if strings.TrimSpace(template) == "" {
		template = DefaultPostTemplate
	}
	return associationKey{
		claim:           slices.Contains(kinds, KindClaim),
		association:     slices.Contains(kinds, KindAssociation),
		bskyPost:        slices.Contains(kinds, KindBskyPost),
		delvePost:       slices.Contains(kinds, KindDelvePost),
		template:        template,
		requireContract: a.RequireContract,
	}
}

func (k associationKey) allows(kind string) bool {
	switch kind {
	case KindClaim:
		return k.claim
	case KindAssociation:
		return k.association
	case KindBskyPost:
		return k.bskyPost
	case KindDelvePost:
		return k.delvePost
	}
	return false
}

func KindOfCollection(collection string) string {
	switch collection {
	case BadgeBlueKeysNSID:
		return KindClaim
	case AssociationNSID:
		return KindAssociation
	case BskyPostNSID:
		return KindBskyPost
	case DelvePostNSID:
		return KindDelvePost
	}
	return ""
}

func collectionOfKind(kind string) string {
	switch kind {
	case KindClaim:
		return BadgeBlueKeysNSID
	case KindAssociation:
		return AssociationNSID
	case KindBskyPost:
		return BskyPostNSID
	case KindDelvePost:
		return DelvePostNSID
	}
	return ""
}

func Matches(kind string, value map[string]any, authority, subject, service, template string) bool {
	if value == nil || authority == "" || subject == "" || authority == subject {
		return false
	}
	if recordType, _ := value["$type"].(string); recordType != collectionOfKind(kind) {
		return false
	}
	switch kind {
	case KindClaim:
		held, ok := claimOf(value)
		return ok && held.operator == authority && held.subject == subject && associationService(held.service, service)
	case KindAssociation:
		recordSubject, _ := value["subject"].(string)
		recordService, _ := value["service"].(string)
		return recordSubject == subject && associationService(recordService, service)
	case KindBskyPost, KindDelvePost:
		return postMatches(value, subject, template)
	}
	return false
}

func postMatches(value map[string]any, subject, template string) bool {
	if strings.TrimSpace(template) == "" {
		template = DefaultPostTemplate
	}
	text, _ := value["text"].(string)
	if strings.TrimSpace(text) != strings.TrimSpace(strings.ReplaceAll(template, "{did}", subject)) {
		return false
	}
	mentions := 0
	matched := false
	facets, _ := value["facets"].([]any)
	for _, facet := range facets {
		entry, _ := facet.(map[string]any)
		features, _ := entry["features"].([]any)
		for _, feature := range features {
			mention, _ := feature.(map[string]any)
			if featureType, _ := mention["$type"].(string); featureType != MentionFacetType {
				continue
			}
			mentions++
			did, _ := mention["did"].(string)
			matched = did == subject
		}
	}
	return mentions == 1 && matched
}

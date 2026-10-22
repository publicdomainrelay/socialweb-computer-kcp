package apigen

import (
	"reflect"
	"strings"
	"testing"
)

const repoRoot = "../.."

func mustModel(t *testing.T) *Model {
	t.Helper()
	m, err := BuildV1alpha1(repoRoot)
	if err != nil {
		t.Fatalf("build the model from api/v1alpha1: %v", err)
	}
	return m
}

func TestRequiredFollowsOmitemptyAndTheOptionalTag(t *testing.T) {
	m := mustModel(t)
	cases := map[string][]string{
		"ClusterSpec":         {"account"},
		"NodeSpec":            {"cluster"},
		"AtprotoIdentitySpec": {"did", "scope"},
		"NodeClaimSpec": {"cluster", "nodeName", "ownerDid", "apiserverPort", "advertiseAddress",
			"tokenFile", "nodePasswordFile", "serverCAFile"},
		"NodeClaimStatus":   nil,
		"NodeClaimContract": nil,
		"RecordRef":         {"uri", "cid"},
		"ContractRelease":   {"event", "payload", "ok"},
	}
	for name, want := range cases {
		s, ok := m.Named[name]
		if !ok {
			t.Errorf("no generated type %s", name)
			continue
		}
		if got := s.Required(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s required = %v, want %v", name, got, want)
		}
	}
}

func TestTheRevisionLockRefusesAChangedSpecUnderAnOldName(t *testing.T) {
	lock := RevisionLock{"v1alpha1-1.nodeclaims.socialweb.computer": "sha256:old"}
	if err := lock.Admit("v1alpha1-1.nodeclaims.socialweb.computer", "sha256:old"); err != nil {
		t.Fatalf("an unchanged spec was refused: %v", err)
	}
	err := lock.Admit("v1alpha1-1.nodeclaims.socialweb.computer", "sha256:new")
	if err == nil || !strings.Contains(err.Error(), "Bump Revision for nodeclaims") {
		t.Fatalf("a changed spec under the old name was admitted: %v", err)
	}
	if err := lock.Admit("v1alpha1-2.nodeclaims.socialweb.computer", "sha256:new"); err != nil {
		t.Fatalf("a new revision was refused: %v", err)
	}
	if len(lock) != 2 {
		t.Fatalf("the lock forgot a revision: %v", lock)
	}
}

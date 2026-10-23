package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
)

const (
	OutcomeCreated   = "created"
	OutcomeFound     = "found present"
	OutcomeAdopted   = "adopted"
	OutcomeUpdated   = "updated"
	OutcomeNotNeeded = "not needed here"
	OutcomeRefused   = "refused"
)

const (
	StepAccount    = "account"
	StepSubuid     = "subuid"
	StepSubgid     = "subgid"
	StepKVM        = "kvm-group"
	StepDelegation = "delegation-drop-in"
	StepAppArmor   = "apparmor-profile"
	StepPassword   = "node-password"
	StepStateDir   = "state-dir"
	StepBinary     = "binary"
	StepConfig     = "config"
	StepUnitFiles  = "unit-files"
	StepLinger     = "linger"
	StepUnits      = "core-units"
	StepGuestImage = "guest-image"
	StepBringUp    = "bring-up"
	StepCheckHost  = "check-host"
	recordMode     = 0o644
)

type Record struct {
	Role        hostcontract.Role `json:"role"`
	Account     string            `json:"account"`
	UID         int               `json:"uid"`
	StateDir    string            `json:"stateDir"`
	UnitPrefix  string            `json:"unitPrefix,omitempty"`
	Handles     []string          `json:"handles,omitempty"`
	Steps       map[string]string `json:"steps"`
	SubidLines  map[string]string `json:"subidLines,omitempty"`
	CreatedDirs []string          `json:"createdDirs,omitempty"`
	UnitFiles   []string          `json:"unitFiles,omitempty"`
	PrevOwner   map[string][2]int `json:"previousOwner,omitempty"`
}

func loadRecord(path string) (*Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, errors.Join(errors.New("install: the install record "+path+" is not valid JSON"), err)
	}
	if r.Steps == nil {
		r.Steps = map[string]string{}
	}
	return &r, nil
}

func loadOrNewRecord(path string, role hostcontract.Role, account, state string) (*Record, bool, error) {
	r, err := loadRecord(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Record{Role: role, Account: account, StateDir: state, UID: -1, Steps: map[string]string{}}, false, nil
	}
	return r, err == nil, err
}

func (r *Record) set(step, outcome string) {
	switch r.Steps[step] {
	case OutcomeCreated, OutcomeAdopted:
		return
	case OutcomeUpdated:
		if outcome != OutcomeCreated {
			return
		}
	}
	r.Steps[step] = outcome
}

func (r *Record) created(step string) bool { return r.Steps[step] == OutcomeCreated }

func (r *Record) addDir(p string) {
	if slices.Contains(r.CreatedDirs, p) {
		return
	}
	r.CreatedDirs = append(r.CreatedDirs, p)
}

func (r *Record) addUnitFile(p string) {
	if slices.Contains(r.UnitFiles, p) {
		return
	}
	r.UnitFiles = append(r.UnitFiles, p)
	sort.Strings(r.UnitFiles)
}

func (r *Record) addHandles(hs []string) {
	seen := map[string]bool{}
	for _, h := range append(append([]string(nil), r.Handles...), hs...) {
		seen[h] = true
	}
	r.Handles = r.Handles[:0]
	for h := range seen {
		r.Handles = append(r.Handles, h)
	}
	sort.Strings(r.Handles)
}

func (r *Record) bytes() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return append(b, '\n')
}

func sameBytes(path string, b []byte) bool {
	cur, err := os.ReadFile(path)
	return err == nil && bytes.Equal(cur, b)
}

func ReadRecord(root, account string) (*Record, error) {
	return loadRecord(filepath.Join(root, RecordPath(account)))
}

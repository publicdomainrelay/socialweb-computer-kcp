package clusterprovider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const (
	RollerStatusFile        = "roller-status.json"
	RollObservedSuffix      = ".observed.json"
	DefaultRollObserveBound = 30 * time.Second

	RollStateWaiting = "waiting"
	RollStateStarted = "started"
)

type RollerStatus struct {
	Provider    string    `json:"provider"`
	PID         int       `json:"pid"`
	PassAt      time.Time `json:"passAt"`
	Clusters    []string  `json:"clusters,omitempty"`
	Error       string    `json:"error,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
}

type RollObserved struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	FirstAt    time.Time `json:"firstAt"`
	ObservedAt time.Time `json:"observedAt"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
}

func RollerStatusPath(stateDir string) string { return filepath.Join(stateDir, RollerStatusFile) }

func RollObservedPath(stateDir, cluster, id string) (string, error) {
	dir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, RollsDirName, id+RollObservedSuffix), nil
}

func readJSONFile(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}

func ReadRollerStatus(stateDir string) (RollerStatus, bool, error) {
	var s RollerStatus
	ok, err := readJSONFile(RollerStatusPath(stateDir), &s)
	return s, ok, err
}

func ReadRollObserved(stateDir, cluster, id string) (RollObserved, bool, error) {
	var o RollObserved
	p, err := RollObservedPath(stateDir, cluster, id)
	if err != nil {
		return o, false, err
	}
	ok, err := readJSONFile(p, &o)
	return o, ok, err
}

func (r *Roller) noteStatus(clusters []string, err error) {
	now := time.Now().UTC()
	r.status.Provider, r.status.PID, r.status.PassAt, r.status.Clusters = r.Name, os.Getpid(), now, clusters
	r.status.Error = ""
	if err != nil {
		r.status.Error, r.status.LastError, r.status.LastErrorAt = err.Error(), err.Error(), now
	}
	if werr := fsx.WriteJSON(RollerStatusPath(r.Stack.StateDir), r.status, 0o600); werr != nil {
		r.log().Warn("cluster provider: writing the roller status", "err", werr.Error())
	}
}

func (r *Roller) noteObserved(key string, req RollRequest, state string, err error) {
	p, perr := RollObservedPath(r.Stack.StateDir, key, req.ID)
	if perr != nil {
		return
	}
	now := time.Now().UTC()
	o := RollObserved{ID: req.ID, Provider: r.Name, FirstAt: now, ObservedAt: now, State: state}
	var prev RollObserved
	if ok, _ := readJSONFile(p, &prev); ok && !prev.FirstAt.IsZero() {
		o.FirstAt = prev.FirstAt
	}
	if err != nil {
		o.Error = err.Error()
	}
	if werr := fsx.WriteJSON(p, o, 0o600); werr != nil {
		r.log().Warn("cluster provider: writing the roll observation", "cluster", key, "roll", req.ID, "err", werr.Error())
	}
}

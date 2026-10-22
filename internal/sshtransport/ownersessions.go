package sshtransport

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

type OwnerSession struct {
	Service string `json:"service"`

	Identifier string `json:"identifier"`

	Password string `json:"password"`
}

func LoadOwnerSessions(path string) (map[string]OwnerSession, string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", fmt.Errorf("sshtransport: the owner sessions file %s: %w", path, err)
	}
	sessions := map[string]OwnerSession{}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, "", "", fmt.Errorf("sshtransport: the owner sessions file %s: %w", path, err)
	}
	if len(sessions) == 0 {
		return nil, "", "", fmt.Errorf("sshtransport: the owner sessions file %s names no account", path)
	}
	dids := make([]string, 0, len(sessions))
	for did := range sessions {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	did := dids[0]
	return sessions, did, sessions[did].Identifier, nil
}

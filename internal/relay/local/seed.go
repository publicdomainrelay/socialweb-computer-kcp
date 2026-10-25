package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	atrelay "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/relay"
)

type seedFile struct {
	Hosts []atrelay.HostInfo `json:"hosts"`
}

func seedHosts(store atrelay.Store, path string) (int, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("relay: reading the seed hosts %s: %w", path, err)
	}
	var seed seedFile
	if err := json.Unmarshal(body, &seed); err != nil {
		return 0, fmt.Errorf("relay: the seed hosts %s are not a listHosts answer: %w", path, err)
	}
	added := 0
	for _, h := range seed.Hosts {
		hostname := strings.ToLower(strings.TrimSpace(h.Hostname))
		if hostname == "" {
			continue
		}
		if _, ok := store.GetHost(hostname); ok {
			continue
		}
		store.PutHost(atrelay.HostInfo{Hostname: hostname, DID: h.DID, State: atrelay.HostIdle, LastSeen: h.LastSeen})
		added++
	}
	return added, nil
}

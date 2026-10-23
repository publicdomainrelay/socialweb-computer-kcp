package local

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/lease"
)

type (
	Slot = lease.Slot

	Slots = lease.Slots
)

type SlotFile struct {
	Path string
}

func (f SlotFile) Slots() (Slots, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return Slots{}, err
	}
	var s Slots
	if err := json.Unmarshal(b, &s); err != nil {
		return Slots{}, fmt.Errorf("local: %s: %w", f.Path, err)
	}
	if s.Slots == nil {
		s.Slots = map[string]Slot{}
	}
	return s, nil
}

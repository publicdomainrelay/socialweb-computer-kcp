package local

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type Slot struct {
	Port int `json:"port"`

	StartedAt time.Time `json:"startedAt,omitzero"`

	ReadyAt time.Time `json:"readyAt,omitzero"`
}

type Slots struct {
	Active string `json:"active"`

	Slots map[string]Slot `json:"slots"`

	Updated time.Time `json:"updated"`
}

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

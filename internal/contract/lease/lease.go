package lease

import "time"

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

type Rolls interface {
	Slots() (Slots, error)
}

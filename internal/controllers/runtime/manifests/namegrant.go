package manifests

import (
	"bytes"
	"encoding/json"
	"errors"
)

type NameGrant struct {
	All   bool
	Names []string
}

func (g NameGrant) IsZero() bool { return !g.All && len(g.Names) == 0 }

func (g NameGrant) MarshalJSON() ([]byte, error) {
	if g.All {
		return []byte("true"), nil
	}
	if len(g.Names) == 0 {
		return []byte("false"), nil
	}
	return json.Marshal(g.Names)
}

func (g *NameGrant) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")), bytes.Equal(b, []byte("false")):
		*g = NameGrant{}
		return nil
	case bytes.Equal(b, []byte("true")):
		*g = NameGrant{All: true}
		return nil
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return errors.New("a grant is true, false or a list of names")
	}
	*g = NameGrant{Names: names}
	return nil
}

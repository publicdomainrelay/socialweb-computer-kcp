package pds

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

const Kind = "Pds"

const (
	RepoKeyDir        = "repo-keys"
	SessionSecretFile = "session-secret"
	RotationKeyFile   = "plc-rotation.key"
)

type Deps struct {
	component.Instance
	Egress component.Egress
}

package infra

import "errors"

//go:generate go run ../../lexgen -err ErrInvalidRecord -include computer.socialweb.infra,computer.socialweb.ephemeralInfra -trim computer.socialweb.

const (
	InfraNSID = "computer.socialweb.infra"

	EphemeralInfraNSID = "computer.socialweb.ephemeralInfra"
)

var ErrInvalidRecord = errors.New("infra: invalid record")

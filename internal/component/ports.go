package component

import (
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component/ports"
)

type PortLease = ports.Lease

type PortRegistry = ports.Registry

type PortRegistryOptions = ports.Options

func OpenPortRegistry(opts PortRegistryOptions) (*PortRegistry, error) {
	return ports.Open(opts)
}

func NewPortRegistry(stateDir string) (*PortRegistry, error) {
	return ports.New(stateDir)
}

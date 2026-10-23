package ssh

import (
	"context"
	"net"
)

const (
	CapabilityRun = "stream.dial"

	ScopeRun = "command"
)

type Account struct {
	DID    string
	Handle string
	PDS    string
}

type PTYSize struct {
	Cols int
	Rows int
}

type CommandIO interface {
	Write(chunk []byte) error

	WriteErr(chunk []byte) error

	OnData(handler func(chunk []byte))

	OnClose(handler func())

	Exit(code int)

	PTY() *PTYSize
}

type Runner interface {
	RunCommand(ctx context.Context, account Account, command string, env map[string]string, io CommandIO) error
}

type Server interface {
	ServeConn(ctx context.Context, conn net.Conn) error

	Shutdown(ctx context.Context) error
}

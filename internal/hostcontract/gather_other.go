//go:build !linux

package hostcontract

import (
	"context"
	"io"
)

type Options struct {
	Role       Role
	StateDir   string
	Self       []string
	PastaPath  string
	AssetCache string
	Getenv     func(string) string
}

const (
	ProbeHoldFlag   = "-probe-hold"
	ProbeUsernsFlag = "-probe-userns"
)

func Gather(ctx context.Context, o Options) Facts {
	return Facts{Role: o.Role, VersionErr: "the Linux host contract has no meaning on this OS"}
}

func GatherRequesterGateway(o Options) Facts {
	return Facts{Role: RoleGateway, VersionErr: "the Linux host contract has no meaning on this OS"}
}

func UsernsChild(stdin io.Reader, stdout io.Writer) int { return 2 }

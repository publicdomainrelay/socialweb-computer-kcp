package hostsvc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	ProcOpHello       = "hello"
	ProcOpStart       = "start"
	ProcOpInspect     = "inspect"
	ProcOpStop        = "stop"
	ProcOpStopSlice   = "stop-slice"
	ProcOpResetFailed = "reset-failed"
	ProcOpList        = "list"
	ProcOpIdentify    = "identify"
	ProcOpCheck       = "check"
	ProcOpWatch       = "watch"
	ProcOpShutdown    = "shutdown"
)

const ProcStopCallTimeout = 3 * time.Minute

const (
	procErrUnsupported = "unsupported"
)

type ProcClient interface {
	Manager
	Hello(ctx context.Context) (ProcHello, error)
	Shutdown(ctx context.Context) error
}

type ProcRequest struct {
	Op       string    `json:"op"`
	Spec     *UnitSpec `json:"spec,omitempty"`
	Name     string    `json:"name,omitempty"`
	Prefix   string    `json:"prefix,omitempty"`
	Slice    string    `json:"slice,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Identity *Identity `json:"identity,omitempty"`
}

type ProcHello struct {
	Instance   string    `json:"instance"`
	PID        int       `json:"pid"`
	StartTicks uint64    `json:"startTicks"`
	BootID     string    `json:"bootID"`
	Started    time.Time `json:"started"`
	State      string    `json:"state"`
	Socket     string    `json:"socket"`
}

type ProcResponse struct {
	Err         string       `json:"err,omitempty"`
	ErrKind     string       `json:"errKind,omitempty"`
	Status      *UnitStatus  `json:"status,omitempty"`
	Statuses    []UnitStatus `json:"statuses,omitempty"`
	Identity    *Identity    `json:"identity,omitempty"`
	Observation *Observation `json:"observation,omitempty"`
	Hello       *ProcHello   `json:"hello,omitempty"`
	Event       *UnitEvent   `json:"event,omitempty"`
}

func ProcError(err error) ProcResponse {
	r := ProcResponse{Err: err.Error()}
	if errors.Is(err, ErrUnsupported) {
		r.ErrKind = procErrUnsupported
	}
	return r
}

func (r ProcResponse) Error() error {
	if r.Err == "" {
		return nil
	}
	if r.ErrKind == procErrUnsupported {
		return fmt.Errorf("%w: %s", ErrUnsupported, r.Err)
	}
	return errors.New(r.Err)
}

func ProcCapabilities() Capabilities {
	return Capabilities{Backend: BackendProc, FDStore: procFDStore}
}

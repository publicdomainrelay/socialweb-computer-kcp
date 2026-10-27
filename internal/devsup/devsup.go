package devsup

import (
	"errors"
	"log/slog"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

const (
	EnvUnit       = "SWC_UNIT"
	EnvDevsup     = "SWC_DEVSUP"
	EnvInvocation = "INVOCATION_ID"
	EnvInstance   = "SWC_INSTANCE"
)

const (
	EnvNotifySocket = "NOTIFY_SOCKET"
	EnvListenFDs    = "LISTEN_FDS"
	EnvListenNames  = "LISTEN_FDNAMES"
)

const (
	DefaultStopTimeout = 30 * time.Second
	RestartDelay       = 100 * time.Millisecond
	StartLimitBurst    = 5
	StartLimitInterval = 10 * time.Second
	OrphanScanInterval = time.Second
	ExitStatusExec     = 203
	CodeExited         = 1
	CodeKilled         = 2
	CodeDumped         = 3
)

type Options struct {
	State    string
	Socket   string
	DieWith  int
	Instance string
	Log      *slog.Logger
}

type Record struct {
	Spec        hostsvc.UnitSpec   `json:"spec"`
	Status      hostsvc.UnitStatus `json:"status"`
	PGID        int                `json:"pgid"`
	LeaderTicks uint64             `json:"leaderTicks"`
	BootID      string             `json:"bootID"`
	Devsup      string             `json:"devsup"`
	Order       int                `json:"order"`
}

type ProcEntry struct {
	PID   int
	PGID  int
	Ticks uint64
}

type SweepVerdict int

const (
	SweepNothing SweepVerdict = iota
	SweepGroup
	SweepRefuseReused
)

type SweepReport struct {
	Unit    string `json:"unit"`
	PGID    int    `json:"pgid"`
	Verdict string `json:"verdict"`
}

var errGone = errors.New("the process is gone")

func (v SweepVerdict) String() string {
	switch v {
	case SweepGroup:
		return "signal the group"
	case SweepRefuseReused:
		return "refused: the leader pid was reused"
	}
	return "nothing left"
}

func SweepDecision(rec Record, table []ProcEntry) SweepVerdict {
	if rec.PGID <= 0 {
		return SweepNothing
	}
	members := 0
	for _, p := range table {
		if p.PID == rec.PGID {
			if p.Ticks == rec.LeaderTicks {
				return SweepGroup
			}
			return SweepRefuseReused
		}
		if p.PGID == rec.PGID {
			members++
		}
	}
	if members > 0 {
		return SweepGroup
	}
	return SweepNothing
}

func CleanSignal(sig syscall.Signal) bool {
	switch sig {
	case syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGPIPE:
		return true
	}
	return false
}

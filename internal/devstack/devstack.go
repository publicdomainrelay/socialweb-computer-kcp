package devstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc/hostsvctest"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/livegate"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/testdirs"
)

const (
	EnvStandIn = "SWC_DEVSTACK_STANDIN"
	MainPkg    = "github.com/johnandersen777/socialweb-computer-kcp/cmd/socialweb-computer"
	DownWait   = 3 * time.Minute
)

type Profile struct {
	Name     string
	StandIns []devinstance.StandIn
	Args     []string
	Env      []string
}

func StandIns(s ...devinstance.StandIn) Profile { return Profile{StandIns: s} }

func Named(name string, args ...string) Profile { return Profile{Name: name, Args: args} }

func IsStandIn() bool { return os.Getenv(EnvStandIn) != "" }

func StandInMain() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer l.Close()
	fmt.Fprintln(os.Stdout, "stand-in", os.Getenv(EnvStandIn), "listening on", l.Addr().String())
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	<-ctx.Done()
	return 0
}

func Listener(role string) devinstance.StandIn {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return devinstance.StandIn{Role: role, Exec: []string{exe, "-test.run=^$"}, Env: []string{EnvStandIn + "=" + role}, Listens: true}
}

type Up struct {
	Instance        string                            `json:"instance"`
	Profile         string                            `json:"profile"`
	StateDir        string                            `json:"stateDir"`
	Socket          string                            `json:"socket"`
	AdminKubeconfig string                            `json:"adminKubeconfig"`
	KcpURL          string                            `json:"kcpURL"`
	KineURL         string                            `json:"kineURL"`
	Devsup          devinstance.Devsup                `json:"devsup"`
	Units           []devinstance.Unit                `json:"units"`
	Core            devinstance.Core                  `json:"core"`
	Seconds         float64                           `json:"seconds"`
	Env             map[string]string                 `json:"env"`
	Executables     map[string]devinstance.Executable `json:"executables"`
}

type Down struct {
	Instance       string  `json:"instance"`
	DevsupStopped  bool    `json:"devsupStopped"`
	DevsupPID      int     `json:"devsupPID"`
	LeftoverKilled []int   `json:"leftoverKilled"`
	Remaining      []int   `json:"remaining"`
	RemovedState   bool    `json:"removedState"`
	RemovedRuntime bool    `json:"removedRuntime"`
	Seconds        float64 `json:"seconds"`
}

type Status struct {
	Instance string              `json:"instance"`
	Found    bool                `json:"found"`
	Record   *devinstance.Record `json:"record"`
	Core     *devinstance.Core   `json:"core"`
	Devsup   struct {
		Reachable bool                 `json:"reachable"`
		Units     []hostsvc.UnitStatus `json:"units"`
		Error     string               `json:"error"`
	} `json:"devsup"`
	Ports map[string]int `json:"ports"`
}

type Restart struct {
	Instance       string           `json:"instance"`
	Before         devinstance.Core `json:"before"`
	After          devinstance.Core `json:"after"`
	UnitsUnchanged bool             `json:"unitsUnchanged"`
	Seconds        float64          `json:"seconds"`
}

type host struct {
	state   string
	xdg     string
	runtime string
}

var (
	buildMu sync.Mutex
	built   string
	hosts   sync.Map
)

func Binary(t *testing.T) string {
	t.Helper()
	buildMu.Lock()
	defer buildMu.Unlock()
	if built != "" {
		if _, err := os.Stat(built); err == nil {
			return built
		}
	}
	dir, err := os.MkdirTemp("", "devstack-bin-")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "socialweb-computer")
	if out, err := exec.Command("go", "build", "-o", bin, MainPkg).CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		t.Fatalf("go build %s: %v\n%s", MainPkg, err, out)
	}
	built = bin
	t.Cleanup(func() {
		buildMu.Lock()
		defer buildMu.Unlock()
		os.RemoveAll(dir)
		built = ""
	})
	return bin
}

func hostOf(t *testing.T) *host {
	if h, ok := hosts.Load(t); ok {
		return h.(*host)
	}
	h := &host{state: filepath.Join(t.TempDir(), "state"), xdg: t.TempDir(), runtime: testdirs.Short(t)}
	hosts.Store(t, h)
	t.Cleanup(func() { hosts.Delete(t) })
	return h
}

type Stack struct {
	T       *testing.T
	ID      string
	Bin     string
	Profile Profile
	Up      Up
	host    *host
	down    bool
}

func Start(t *testing.T, p Profile) *Stack {
	t.Helper()
	livegate.Short(t, "devstack runs devsup, a dev core and their units as real processes (proc backend, no systemd)")
	if p.Name == "" && len(p.StandIns) == 0 {
		t.Fatal("devstack: a profile names either a dev profile or its stand-ins")
	}
	s := &Stack{T: t, ID: "t" + hostsvctest.RandomKey(), Bin: Binary(t), Profile: p, host: hostOf(t)}
	t.Cleanup(func() {
		if !s.down {
			if _, err := s.run(nil, "dev", "down", "--instance", s.ID); err != nil {
				t.Errorf("devstack cleanup: %v", err)
			}
		}
	})
	s.Up = s.UpAgain(t)
	t.Logf("devstack %s up in %.1fs: devsup %d core %d units %d", s.ID, s.Up.Seconds, s.Up.Devsup.PID, s.Up.Core.PID, len(s.Up.Units))
	return s
}

func (s *Stack) upArgs() ([]string, error) {
	args := []string{"dev", "up", "--instance", s.ID, "--die-with", strconv.Itoa(os.Getpid())}
	if s.Profile.Name != "" {
		args = append(args, "--profile", s.Profile.Name)
	}
	if len(s.Profile.StandIns) > 0 {
		b, err := json.Marshal(s.Profile.StandIns)
		if err != nil {
			return nil, err
		}
		f := filepath.Join(s.host.xdg, s.ID+"-stand-ins.json")
		if err := os.WriteFile(f, b, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--stand-ins", f)
	}
	return append(args, s.Profile.Args...), nil
}

func (s *Stack) UpAgain(t *testing.T) Up {
	t.Helper()
	args, err := s.upArgs()
	if err != nil {
		t.Fatal(err)
	}
	var u Up
	if _, err := s.run(&u, args...); err != nil {
		t.Fatal(err)
	}
	return u
}

func (s *Stack) Down(t *testing.T) Down {
	t.Helper()
	var d Down
	if _, err := s.run(&d, "dev", "down", "--instance", s.ID); err != nil {
		t.Fatal(err)
	}
	s.down = true
	return d
}

func (s *Stack) Status(t *testing.T) Status {
	t.Helper()
	var st Status
	if _, err := s.run(&st, "dev", "status", "--instance", s.ID); err != nil {
		t.Fatal(err)
	}
	return st
}

func (s *Stack) RestartCore(t *testing.T) Restart {
	t.Helper()
	var r Restart
	if _, err := s.run(&r, "dev", "restart", "core", "--instance", s.ID, "--binary", s.Bin); err != nil {
		t.Fatal(err)
	}
	return r
}

func (s *Stack) WaitCore(t *testing.T, after int, within time.Duration) devinstance.Core {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var c devinstance.Core
		if err := devinstance.ReadJSON(filepath.Join(s.Up.StateDir, "core.json"), &c); err == nil && c.Generation > after && !c.AdoptedAt.IsZero() {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("no core generation after %d of %s within %s", after, s.ID, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (s *Stack) Env() []string {
	env := append(os.Environ(), "XDG_RUNTIME_DIR="+s.host.runtime, "XDG_STATE_HOME="+s.host.xdg)
	return append(env, s.Profile.Env...)
}

func (s *Stack) Run(out any, args ...string) ([]byte, error) { return s.run(out, args...) }

func (s *Stack) run(out any, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DownWait)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.Bin, append(args, "--state-dir", s.host.state)...)
	cmd.Env = s.Env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%v: %v\n%s\n%s", args, err, stdout.String(), stderr.String())
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return stdout.Bytes(), fmt.Errorf("%v: %v\n%s", args, err, stdout.String())
		}
	}
	return stdout.Bytes(), nil
}

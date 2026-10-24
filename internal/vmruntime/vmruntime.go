package vmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestimage"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

const (
	UnitKind          = "socialweb-vm-"
	SliceKind         = "socialweb-b-"
	DefaultInnerID    = 1000
	DefaultBootTime   = 30 * time.Second
	DefaultStopGrace  = 10 * time.Second
	DefaultConsoleCap = 8 << 20
	VMMOverheadMiB    = 192
	StopTimeout       = 10 * time.Second
	RefFile           = "ref.json"
	GrantsFile        = "grants.json"
	ConsoleFile       = "console.log"
	VMMLogFile        = "vmm.log"
	RootfsFile        = "rootfs.ext4"
	SeedFile          = "cidata.img"
	ConfigFile        = "fc.json"
	ParamsFile        = "params.json"
	SocketFile        = "api.sock"
)

var ErrUnsupported = errors.New("vmruntime: no VM runtime on this OS (plans 0011 and 0012 own the other hosts)")

type (
	Verdict    = hostsvc.Verdict
	LostReason = hostsvc.LostReason
)

type Executables struct {
	Stage       string
	Pasta       string
	VMExec      string
	Firecracker string
}

type Config struct {
	Manager     hostsvc.Manager
	StateDir    string
	RuntimeDir  string
	Exec        Executables
	ImageSource string
	InnerID     int
	BootTimeout time.Duration
	StopGrace   time.Duration
	ConsoleCap  int64
	Binaries    []string
	ImageOut    io.Writer
	ImageTTY    bool
}

type Capabilities struct {
	Backend        hostsvc.Backend `json:"backend"`
	Hypervisor     string          `json:"hypervisor"`
	EnforcesLimits bool            `json:"enforcesLimits"`
	GracefulStop   bool            `json:"gracefulStop"`
}

type VMSpec struct {
	Handle   string `json:"handle"`
	Name     string `json:"name"`
	UserData string `json:"userData"`
	MetaData string `json:"metaData,omitempty"`
	Vcpu     int    `json:"vcpu,omitempty"`
	MemMiB   int    `json:"memMiB,omitempty"`
	DiskMiB  int    `json:"diskMiB,omitempty"`

	HostPorts []int `json:"hostPorts,omitempty"`
}

type Prepared struct {
	Spec      VMSpec `json:"spec"`
	Unit      string `json:"unit"`
	WorkDir   string `json:"workDir"`
	SocketDir string `json:"socketDir"`
	Socket    string `json:"socket"`
	VMMID     string `json:"vmmID"`
	ImageDir  string `json:"imageDir"`
	Vcpu      int    `json:"vcpu"`
	MemMiB    int    `json:"memMiB"`
}

type RuntimeRef struct {
	Backend      hostsvc.Backend `json:"backend"`
	Hypervisor   string          `json:"hypervisor"`
	Handle       string          `json:"handle"`
	Name         string          `json:"name"`
	Unit         string          `json:"unit"`
	InvocationID string          `json:"invocationID"`
	PID          int             `json:"pid"`
	StartTicks   uint64          `json:"startTicks"`
	StartTime    time.Time       `json:"startTime"`
	HostBootID   string          `json:"hostBootID"`
	Manager      string          `json:"manager"`
	VMMID        string          `json:"vmmID"`
	Slot         int             `json:"slot"`
	VMMUID       int             `json:"vmmUID"`
	WorkDir      string          `json:"workDir"`
	Socket       string          `json:"socket"`
	GuestAddress string          `json:"guestAddress"`
	HostAddress  string          `json:"hostAddress"`
	ImageSHA256  string          `json:"imageSha256"`
}

type Observation struct {
	Verdict string     `json:"verdict"`
	Reason  LostReason `json:"reason,omitempty"`
	Detail  string     `json:"detail,omitempty"`
	Stopped bool       `json:"stopped,omitempty"`
	State   string     `json:"state,omitempty"`
}

type StopResult struct {
	Graceful bool          `json:"graceful"`
	Elapsed  time.Duration `json:"elapsed"`
	Result   string        `json:"result,omitempty"`
}

type Listed struct {
	Name    string             `json:"name"`
	Unit    string             `json:"unit"`
	WorkDir string             `json:"workDir,omitempty"`
	Status  hostsvc.UnitStatus `json:"status"`
	Loaded  bool               `json:"loaded"`
}

type Placer func(ctx context.Context, slot int, spec hostsvc.UnitSpec) error

type Runtime interface {
	Capabilities() Capabilities
	Prepare(ctx context.Context, spec VMSpec) (Prepared, error)
	Placed(handle, name string) Prepared
	Reserve(ctx context.Context, p Prepared, place Placer) (int, error)
	Await(ctx context.Context, p Prepared, slot int) (RuntimeRef, error)
	Shutdown(ctx context.Context, ref RuntimeRef, grace time.Duration) (bool, error)
	StopGrace() time.Duration
	Start(ctx context.Context, p Prepared) (RuntimeRef, error)
	Inspect(ctx context.Context, ref RuntimeRef) (Observation, error)
	Stop(ctx context.Context, ref RuntimeRef) (StopResult, error)
	List(ctx context.Context, handle string) ([]Listed, error)
	Cleanup(ctx context.Context, handle, name string) error
	Watch(ctx context.Context, handle string) (<-chan hostsvc.UnitEvent, error)
	Load(handle, name string) (RuntimeRef, error)
	CollectImages(ctx context.Context, keep map[string]string) (guestimage.GCReport, error)
}

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func ValidateSpec(s VMSpec) error {
	if s.Handle == "" {
		return fmt.Errorf("vmruntime: the VM names no handle, and the handle is what keeps two bidders' VMs apart on one account")
	}
	if len(s.Name) > 48 || !namePattern.MatchString(s.Name) {
		return fmt.Errorf("vmruntime: VM name %q is not [a-z0-9-], 1-48 characters", s.Name)
	}
	if s.UserData == "" {
		return fmt.Errorf("vmruntime: VM %s has no user_data; the user_data is the whole guest", s.Name)
	}
	if s.Vcpu < 0 || s.MemMiB < 0 || s.DiskMiB < 0 {
		return fmt.Errorf("vmruntime: VM %s asks for a negative size", s.Name)
	}
	for _, p := range s.HostPorts {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("vmruntime: VM %s names host port %d, which is not a TCP port", s.Name, p)
		}
	}
	return nil
}

func HandleID(handle string) string {
	sum := sha256.Sum256([]byte(handle))
	return hex.EncodeToString(sum[:])[:10]
}

func UnitPrefix(handle string) string { return UnitKind + HandleID(handle) + "-" }

func UnitName(handle, name string) string { return UnitPrefix(handle) + name + hostsvc.ServiceSuffix }

func SliceName(handle string) string { return SliceKind + HandleID(handle) + ".slice" }

func VMMID(handle, name string) string { return HandleID(handle) + "-" + name }

func WorkDir(stateDir, handle, name string) string {
	return filepath.Join(stateDir, "handles", HandleID(handle), "vms", name)
}

func VMsDir(stateDir, handle string) string {
	return filepath.Join(stateDir, "handles", HandleID(handle), "vms")
}

func SocketDir(runtimeDir, handle, name string) string {
	sum := sha256.Sum256([]byte(UnitName(handle, name)))
	return filepath.Join(runtimeDir, "swvm", hex.EncodeToString(sum[:])[:16])
}

func ImageTarget() string {
	switch runtime.GOARCH {
	case "amd64":
		return "firecracker-x86_64"
	case "arm64":
		return "firecracker-aarch64"
	}
	return ""
}

func GracefulStop() bool { return runtime.GOARCH == "amd64" }

var ExecutableNames = []string{"swc-userns-stage", "pasta", "socialweb-vmexec", "firecracker"}

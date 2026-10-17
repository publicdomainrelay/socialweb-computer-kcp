package devinstance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

const MaxSocketPath = 107

const basenameChars = 16

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

var ErrRoot = errors.New("devinstance: dev refuses to run as root (uid 0): dev mode needs no root; production uses install")

type Env interface {
	LookupEnv(key string) (string, bool)
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > basenameChars {
		out = strings.Trim(out[:basenameChars], "-")
	}
	if out == "" {
		out = "dev"
	}
	return out
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

func WorktreeRoot(dir string) string {
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

func within(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func readableName(root string, env Env) string {
	task, _ := env.LookupEnv("TASK_NAME")
	taskRoot, _ := env.LookupEnv("TASK_ROOT")
	if task != "" && taskRoot != "" {
		if real, err := filepath.EvalSymlinks(taskRoot); err == nil && within(root, real) {
			return sanitize(task)
		}
	}
	return sanitize(filepath.Base(root))
}

func ID(name, cwd string, env Env) (string, error) {
	if name != "" {
		if !namePattern.MatchString(name) {
			return "", fmt.Errorf("devinstance: instance %q: want lower-case letters, digits and dashes, at most 40", name)
		}
		return name, nil
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	root := WorktreeRoot(real)
	return readableName(root, env) + "-" + hash8(root), nil
}

func Short(id string) string { return hash8("swc-instance:" + id) }

func StateRoot(option string, env Env) (string, error) {
	if option != "" {
		return filepath.Abs(option)
	}
	if v, ok := env.LookupEnv("TASK_STATE_DIR"); ok && v != "" {
		return filepath.Join(v, "socialweb-computer"), nil
	}
	if v, ok := env.LookupEnv("XDG_STATE_HOME"); ok && v != "" {
		return filepath.Join(v, "socialweb-computer"), nil
	}
	home, ok := env.LookupEnv("HOME")
	if !ok || home == "" {
		return "", errors.New("devinstance: neither state-dir, TASK_STATE_DIR, XDG_STATE_HOME nor HOME is set")
	}
	return filepath.Join(home, ".local", "state", "socialweb-computer"), nil
}

func RuntimeRoot(env Env) (string, error) {
	v, ok := env.LookupEnv("XDG_RUNTIME_DIR")
	if !ok || v == "" {
		return "", errors.New("devinstance: XDG_RUNTIME_DIR is unset; devsup sockets live there (a login session or linger provides it)")
	}
	return filepath.Join(v, "swc"), nil
}

type Layout struct {
	ID         string `json:"id"`
	Short      string `json:"short"`
	StateDir   string `json:"stateDir"`
	RuntimeDir string `json:"runtimeDir"`
	Socket     string `json:"socket"`
	SupState   string `json:"supState"`
	Record     string `json:"record"`
	CoreRecord string `json:"coreRecord"`
	Lock       string `json:"lock"`
	CoreLock   string `json:"coreLock"`
	Logs       string `json:"logs"`
	KineDir    string `json:"kineDir"`
	KcpRoot    string `json:"kcpRoot"`
	Prefix     string `json:"prefix,omitempty"`
}

func NewLayout(id, stateRoot, runtimeRoot string) (Layout, error) {
	short := Short(id)
	state := filepath.Join(DevRoot(stateRoot), id)
	rt := filepath.Join(runtimeRoot, short)
	l := Layout{
		ID: id, Short: short, StateDir: state, RuntimeDir: rt,
		Socket:     filepath.Join(rt, "sup.sock"),
		SupState:   filepath.Join(state, "sup"),
		Record:     filepath.Join(state, "instance.json"),
		CoreRecord: filepath.Join(state, "core.json"),
		Lock:       filepath.Join(state, "instance.lock"),
		CoreLock:   filepath.Join(state, "core.lock"),
		Logs:       filepath.Join(state, "logs"),
		KineDir:    filepath.Join(state, "kcp", "kine-a"),
		KcpRoot:    filepath.Join(state, "kcp", "slot-a"),
	}
	if n := len(l.Socket); n > MaxSocketPath {
		return Layout{}, fmt.Errorf("devinstance: socket path %s is %d bytes, over the %d-byte sun_path limit", l.Socket, n, MaxSocketPath)
	}
	return l, nil
}

func (l Layout) UnitPrefix() string {
	if l.Prefix != "" {
		return l.Prefix
	}
	return hostsvc.NamePrefix
}

func (l Layout) Names() table.Names { return table.Names{Prefix: l.UnitPrefix(), Key: l.Short} }

func (l Layout) KineUnit() string { return l.Names().Kine(table.SlotA) }

func (l Layout) KcpUnit() string { return l.Names().Slot(table.SlotA) }

func (l Layout) CoreUnit() string { return l.Names().DevCore() }

func (l Layout) KcpSlice() string { return l.Names().Slice() }

func (l Layout) CoreSlice() string { return l.Names().CoreSlice() }

func (l Layout) AdminKubeconfig() string { return filepath.Join(l.KcpRoot, "admin.kubeconfig") }

type Executable struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

type Unit struct {
	Role      string           `json:"role"`
	Unit      string           `json:"unit"`
	Identity  hostsvc.Identity `json:"identity"`
	Port      int              `json:"port"`
	Endpoint  string           `json:"endpoint"`
	Adopted   bool             `json:"adopted"`
	PlacedAt  time.Time        `json:"placedAt"`
	PortAt    time.Time        `json:"portAt"`
	ReadyAt   time.Time        `json:"readyAt,omitzero"`
	RSSKiB    int64            `json:"rssKiB,omitempty"`
	StartedTo string           `json:"startToReady,omitempty"`
}

type StandIn struct {
	Role    string   `json:"role"`
	Exec    []string `json:"exec"`
	Env     []string `json:"env,omitempty"`
	Listens bool     `json:"listens,omitempty"`
}

type Devsup struct {
	Instance   string `json:"instance"`
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"startTicks"`
	BootID     string `json:"bootID"`
	Adopted    bool   `json:"adopted"`
}

type Record struct {
	Layout          Layout                `json:"layout"`
	Worktree        string                `json:"worktree,omitempty"`
	Profile         string                `json:"profile"`
	Backend         hostsvc.Backend       `json:"backend"`
	BaseWorkspace   string                `json:"baseWorkspace"`
	SharedKcp       string                `json:"sharedKcp,omitempty"`
	Created         time.Time             `json:"created"`
	Updated         time.Time             `json:"updated"`
	Devsup          Devsup                `json:"devsup"`
	StandIns        []StandIn             `json:"standIns,omitempty"`
	Units           []Unit                `json:"units"`
	AdminKubeconfig string                `json:"adminKubeconfig"`
	KcpURL          string                `json:"kcpURL"`
	KineURL         string                `json:"kineURL"`
	Executables     map[string]Executable `json:"executables"`
	SourceRoot      string                `json:"sourceRoot,omitempty"`
	OpenBao         *OpenBao              `json:"openbao,omitempty"`
	Ready           bool                  `json:"ready"`
	Host            bool                  `json:"host,omitempty"`
	Parent          *Parent               `json:"parent,omitempty"`
	VM              *VMRuntime            `json:"vm,omitempty"`
	Gateway         *GatewayChild         `json:"gateway,omitempty"`
}

type GatewayChild struct {
	SpecFile            string `json:"specFile"`
	BootstrapCluster    string `json:"bootstrapCluster,omitempty"`
	BootstrapAccountDID string `json:"bootstrapAccountDid,omitempty"`
}

type VMRuntime struct {
	Handle      string            `json:"handle"`
	Backend     hostsvc.Backend   `json:"backend"`
	ImageSource string            `json:"imageSource,omitempty"`
	StopGrace   string            `json:"stopGrace,omitempty"`
	RuntimeDir  string            `json:"runtimeDir"`
	Executables map[string]string `json:"executables"`
}

type OpenBao struct {
	Address   string `json:"address"`
	TokenFile string `json:"tokenFile"`
}

func (r *Record) Unit(role string) *Unit {
	for i := range r.Units {
		if r.Units[i].Role == role {
			return &r.Units[i]
		}
	}
	return nil
}

func (r *Record) SetUnit(u Unit) {
	if cur := r.Unit(u.Role); cur != nil {
		*cur = u
		return
	}
	r.Units = append(r.Units, u)
}

type Adoption struct {
	Role         string `json:"role"`
	Unit         string `json:"unit"`
	InvocationID string `json:"invocationID"`
	PID          int    `json:"pid"`
	Adopted      bool   `json:"adopted"`
}

type Core struct {
	Generation   int        `json:"generation"`
	PID          int        `json:"pid"`
	InvocationID string     `json:"invocationID"`
	StartedAt    time.Time  `json:"startedAt"`
	AdoptedAt    time.Time  `json:"adoptedAt"`
	Adoptions    []Adoption `json:"adoptions"`
	LastCheck    time.Time  `json:"lastCheck"`
	Verdicts     []string   `json:"verdicts"`
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func WriteJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func RefuseRoot(euid int) error {
	if euid == 0 {
		return ErrRoot
	}
	return nil
}

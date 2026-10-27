package kcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
)

type Shape string

const (
	ShapeSingle    Shape = "single"
	ShapeBlueGreen Shape = "blue-green"
	SlotA                = "a"
	SlotB                = "b"
)

const FrontStopTimeout = 10 * time.Second

var SharedFiles = []string{"apiserver.crt", "apiserver.key", "sa.key", ".admin-token-store", "admin.kubeconfig"}

func SlotNames(s Shape) []string {
	if s == ShapeBlueGreen {
		return []string{SlotA, SlotB}
	}
	return []string{SlotA}
}

func Other(slot string) string {
	if slot == SlotA {
		return SlotB
	}
	return SlotA
}

type SlotRecord struct {
	Unit         string    `json:"unit"`
	Root         string    `json:"root"`
	Port         int       `json:"port"`
	PID          int       `json:"pid"`
	InvocationID string    `json:"invocationID"`
	Exec         string    `json:"exec,omitempty"`
	Kine         string    `json:"kine,omitempty"`
	SHA256       string    `json:"sha256,omitempty"`
	Version      string    `json:"version,omitempty"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	ReadyAt      time.Time `json:"readyAt,omitzero"`
}

type FrontRecord struct {
	Unit    string `json:"unit"`
	Address string `json:"address"`
	Port    int    `json:"port"`
	Control string `json:"control"`
	PID     int    `json:"pid"`
}

func (f FrontRecord) URL() string {
	return fmt.Sprintf("https://127.0.0.1:%d", f.Port)
}

type Slots struct {
	Shape   Shape                 `json:"shape"`
	Active  string                `json:"active"`
	Slots   map[string]SlotRecord `json:"slots"`
	Front   FrontRecord           `json:"front"`
	KineURL string                `json:"kineURL,omitempty"`
	Updated time.Time             `json:"updated"`
}

func ReadSlots(path string) (Slots, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Slots{}, err
	}
	var s Slots
	if err := json.Unmarshal(b, &s); err != nil {
		return Slots{}, fmt.Errorf("kcpserver: %s: %w", path, err)
	}
	if s.Slots == nil {
		s.Slots = map[string]SlotRecord{}
	}
	return s, nil
}

func WriteSlots(path string, s Slots) error {
	s.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func missingShared(dir string) []string {
	var missing []string
	for _, f := range SharedFiles {
		if fi, err := os.Stat(filepath.Join(dir, f)); err != nil || fi.Size() == 0 {
			missing = append(missing, f)
		}
	}
	return missing
}

func HasShared(shared string) bool { return len(missingShared(shared)) == 0 }

func SaveShared(root, shared string) error {
	if missing := missingShared(root); len(missing) > 0 {
		return fmt.Errorf("kcpserver: slot root %s lacks %v; the shared material is all five files or none", root, missing)
	}
	if err := os.MkdirAll(shared, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(shared, 0o700); err != nil {
		return err
	}
	for _, f := range SharedFiles {
		if err := copyFile(filepath.Join(root, f), filepath.Join(shared, f)); err != nil {
			return fmt.Errorf("kcpserver: saving %s: %w", f, err)
		}
	}
	return nil
}

func SeedSlot(shared, root string) error {
	if missing := missingShared(shared); len(missing) > 0 {
		return fmt.Errorf("kcpserver: shared material %s lacks %v: a slot started without admin.kubeconfig next to .admin-token-store refuses to start, and one without the rest serves another CA", shared, missing)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	for _, f := range SharedFiles {
		if err := copyFile(filepath.Join(shared, f), filepath.Join(root, f)); err != nil {
			return fmt.Errorf("kcpserver: seeding %s into %s: %w", f, root, err)
		}
	}
	return nil
}

type FrontConfig struct {
	Unit    string
	Slice   string
	Exe     string
	Owner   string
	Address string
	Control string
	State   string
	Log     string
	FDStore bool
}

const FrontMemoryMax = 256 << 20

func FrontSpec(c FrontConfig) (hostsvc.UnitSpec, error) {
	if !filepath.IsAbs(c.Exe) {
		return hostsvc.UnitSpec{}, fmt.Errorf("kcpserver: the front executable must be an absolute path, got %q", c.Exe)
	}
	if c.Owner == "" || c.Address == "" || c.Control == "" || c.State == "" {
		return hostsvc.UnitSpec{}, errors.New("kcpserver: a front needs its owner, address, control socket and state file")
	}
	spec := hostsvc.UnitSpec{
		Name:        c.Unit,
		Description: "L4 front of kcp for " + c.Owner,
		Exec: []string{c.Exe, "internal", "apiserver-front", "--owner", c.Owner, "--kcp-front-address", c.Address,
			"--front-control", c.Control, "--front-state", c.State},
		WorkDir:     filepath.Dir(c.State),
		Slice:       c.Slice,
		Restart:     hostsvc.RestartAlways,
		KillSignal:  syscall.SIGTERM,
		StopTimeout: FrontStopTimeout,
		Limits:      hostsvc.Limits{MemoryMax: FrontMemoryMax},
	}
	if c.FDStore {
		spec.Exec = append(spec.Exec, "--front-fd-store")
		spec.FDStore = 1
	}
	if c.Log != "" {
		spec.Output = hostsvc.Output{StdoutFile: c.Log, StderrFile: c.Log}
	}
	return spec, nil
}

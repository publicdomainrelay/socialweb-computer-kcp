//go:build linux

package vmruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/fcapi"
)

const (
	QEMUComm       = "qemu-system-x86"
	QEMUExtraTasks = 64
)

type vmmAPI interface {
	Describe(ctx context.Context) (fcapi.Instance, error)
	SendCtrlAltDel(ctx context.Context) error
}

func apiFor(vmm, socket string) vmmAPI {
	if vmm == VMMFirecracker {
		return fcapi.NewClient(socket, 2*time.Second)
	}
	return qmpClient{socket: socket, timeout: 2 * time.Second}
}

func qemuRoot(exe string) string { return filepath.Dir(filepath.Dir(filepath.Dir(exe))) }

func qemuDataDir(exe string) string { return filepath.Join(qemuRoot(exe), "usr", "share", "qemu") }

func qemuLibDir(exe string) string {
	d := filepath.Join(qemuRoot(exe), "usr", "lib", "sys")
	if st, err := os.Stat(d); err == nil && st.IsDir() {
		return d
	}
	return ""
}

func qemuEnv(exe string) []string {
	if d := qemuLibDir(exe); d != "" {
		return []string{"LD_LIBRARY_PATH=" + d}
	}
	return nil
}

func qemuArgv(exe, vmmID string, c fcapi.Config, socket string) ([]string, error) {
	for _, d := range c.Drives {
		if strings.Contains(d.PathOnHost, ",") {
			return nil, fmt.Errorf("vmruntime: drive path %s holds a comma, which QEMU reads as an option separator", d.PathOnHost)
		}
	}
	argv := []string{QEMUExecutable, "-name", vmmID,
		"-M", "microvm,acpi=off", "-accel", "kvm", "-cpu", "host",
		"-smp", strconv.Itoa(c.MachineConfig.VcpuCount), "-m", strconv.Itoa(c.MachineConfig.MemSizeMiB),
		"-nodefaults", "-no-user-config", "-display", "none", "-no-reboot",
		"-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
		"-L", qemuDataDir(exe), "-serial", "stdio", "-device", "i8042",
		"-kernel", c.BootSource.KernelImagePath, "-append", c.BootSource.BootArgs,
		"-qmp", "unix:" + socket + ",server=on,wait=off"}
	for _, d := range c.Drives {
		drive := "id=" + d.DriveID + ",file=" + d.PathOnHost + ",format=raw,if=none"
		if d.IsReadOnly {
			drive += ",readonly=on"
		}
		argv = append(argv, "-drive", drive, "-device", "virtio-blk-device,drive="+d.DriveID)
	}
	for _, n := range c.NetworkInterfaces {
		argv = append(argv, "-netdev", "tap,id="+n.IfaceID+",ifname="+n.HostDevName+",script=no,downscript=no",
			"-device", "virtio-net-device,netdev="+n.IfaceID+",mac="+n.GuestMAC)
	}
	return argv, nil
}

type qmpClient struct {
	socket  string
	timeout time.Duration
}

type qmpReply struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
	Event string `json:"event"`
}

func (q qmpClient) run(ctx context.Context, cmds ...map[string]any) ([]json.RawMessage, error) {
	d := net.Dialer{Timeout: q.timeout}
	c, err := d.DialContext(ctx, "unix", q.socket)
	if err != nil {
		return nil, fmt.Errorf("qmp: dialing %s: %w", q.socket, err)
	}
	defer c.Close()
	deadline := time.Now().Add(q.timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	c.SetDeadline(deadline)
	dec, enc := json.NewDecoder(c), json.NewEncoder(c)
	var greeting map[string]json.RawMessage
	if err := dec.Decode(&greeting); err != nil {
		return nil, fmt.Errorf("qmp: greeting on %s: %w", q.socket, err)
	}
	if _, ok := greeting["QMP"]; !ok {
		return nil, fmt.Errorf("qmp: %s does not greet as QMP", q.socket)
	}
	var out []json.RawMessage
	for _, cmd := range append([]map[string]any{{"execute": "qmp_capabilities"}}, cmds...) {
		if err := enc.Encode(cmd); err != nil {
			return nil, fmt.Errorf("qmp: %v: %w", cmd["execute"], err)
		}
		for {
			var r qmpReply
			if err := dec.Decode(&r); err != nil {
				return nil, fmt.Errorf("qmp: %v: %w", cmd["execute"], err)
			}
			if r.Event != "" {
				continue
			}
			if r.Error != nil {
				return nil, fmt.Errorf("qmp: %v: %s: %s", cmd["execute"], r.Error.Class, r.Error.Desc)
			}
			out = append(out, r.Return)
			break
		}
	}
	return out[1:], nil
}

func (q qmpClient) Describe(ctx context.Context) (fcapi.Instance, error) {
	out, err := q.run(ctx, map[string]any{"execute": "query-name"}, map[string]any{"execute": "query-status"})
	if err != nil {
		return fcapi.Instance{}, err
	}
	var name struct {
		Name string `json:"name"`
	}
	var status struct {
		Status  string `json:"status"`
		Running bool   `json:"running"`
	}
	if err := errors.Join(json.Unmarshal(out[0], &name), json.Unmarshal(out[1], &status)); err != nil {
		return fcapi.Instance{}, fmt.Errorf("qmp: %w", err)
	}
	in := fcapi.Instance{ID: name.Name, State: status.Status}
	if status.Running {
		in.State = fcapi.StateRunning
	}
	return in, nil
}

func (q qmpClient) SendCtrlAltDel(ctx context.Context) error {
	keys := []map[string]string{{"type": "qcode", "data": "ctrl"}, {"type": "qcode", "data": "alt"}, {"type": "qcode", "data": "delete"}}
	_, err := q.run(ctx, map[string]any{"execute": "send-key", "arguments": map[string]any{"keys": keys}})
	return err
}

//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestnet"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "socialweb-vmexec: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: socialweb-vmexec <params.json>: pasta runs this inside the guest's namespace with the parameters the VM runtime wrote")
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		return err
	}
	var p guestnet.VMExec
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("%s: %w", args[1], err)
	}
	if p.VMM == "" || len(p.Argv) == 0 || p.Console == "" {
		return fmt.Errorf("%s names VMM %q, argv %q and console %q; all three are needed", args[1], p.VMM, p.Argv, p.Console)
	}
	if err := guestnet.Wire(p.Wiring); err != nil {
		return err
	}
	console, err := os.OpenFile(p.Console, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("opening the console file: %w", err)
	}
	if err := syscall.Dup3(int(console.Fd()), 1, 0); err != nil {
		return fmt.Errorf("console onto stdout: %w", err)
	}
	if err := syscall.Dup3(int(console.Fd()), 2, 0); err != nil {
		return fmt.Errorf("console onto stderr: %w", err)
	}
	syscall.Umask(0)
	return syscall.Exec(p.VMM, p.Argv, append([]string{"TZ=UTC"}, p.Env...))
}

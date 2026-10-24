package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestagent"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/notify"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 && args[0] == "wait-secrets" {
		return waitSecrets(args[1:])
	}
	fs := flag.NewFlagSet("socialweb-guest-agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configDir := fs.String("config-dir", guestagent.DefaultConfigDir, "the directory of tunnels.json and bidder.json")
	stateDir := fs.String("state-dir", guestagent.DefaultStateDir, "the directory of the subscription cursor")
	login := fs.String("login", guestagent.DefaultLoginHelper, "the guest login helper that prints a token for an audience")
	level := fs.String("log-level", "info", "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.TrimSpace(*level))); err != nil {
		fmt.Fprintf(os.Stderr, "socialweb-guest-agent: -log-level %q: %v\n", *level, err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	cfg, err := guestagent.LoadConfig(*configDir, *stateDir, *login)
	if err != nil {
		log.Error("the guest agent config is invalid", "err", err.Error())
		return 1
	}
	agent, err := guestagent.New(cfg, log)
	if err != nil {
		log.Error("the guest agent cannot start", "err", err.Error())
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ready := func() {
		if err := notify.Ready(); err != nil {
			log.Warn("sd_notify READY failed", "err", err.Error())
		}
	}
	if err := agent.Run(ctx, ready); err != nil {
		log.Error("the guest agent failed", "err", err.Error())
		return 1
	}
	return 0
}

func waitSecrets(args []string) int {
	fs := flag.NewFlagSet("socialweb-guest-agent wait-secrets", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", guestagent.DefaultStateDir, "the directory of secrets.ready")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "socialweb-guest-agent: waiting for %s/%s\n", *stateDir, guestagent.SecretsReadyFile)
	if err := guestagent.WaitSecrets(ctx, *stateDir); err != nil {
		fmt.Fprintf(os.Stderr, "socialweb-guest-agent: wait-secrets: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "socialweb-guest-agent: the secrets are written\n")
	return 0
}

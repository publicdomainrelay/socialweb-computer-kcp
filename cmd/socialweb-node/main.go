package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/nodeagent"
)

const defaultConfigPath = "/etc/socialweb/node/join.json"

const usage = `socialweb-node - the guest node agent

  socialweb-node join [--config PATH]
  socialweb-node cni [--config PATH]
  socialweb-node diagnose [--config PATH]
  socialweb-node hostname [--config PATH]
  socialweb-node dns-link [--config PATH]
  socialweb-node masquerade [--config PATH]
  socialweb-node install-meshd [--config PATH]
  socialweb-node units install [--config PATH] [--root DIR]
  socialweb-node units start [--config PATH]

The config path defaults to $SOCIALWEB_NODE_CONFIG or ` + defaultConfigPath + `.
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	command := args[0]
	args = args[1:]
	fs := flag.NewFlagSet("socialweb-node "+command, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", configPathFromEnv(), "the join config file")
	root := fs.String("root", "", "the root under which the units are installed")
	if command == "units" {
		if len(args) == 0 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		mode := args[0]
		if err := fs.Parse(args[1:]); err != nil {
			return flagCode(err)
		}
		cfg, err := nodeagent.LoadConfig(*configPath)
		if err != nil {
			return fail(command, err)
		}
		log := logger()
		switch mode {
		case "install":
			if err := nodeagent.InstallUnits(*root, cfg); err != nil {
				return fail(command+" install", err)
			}
			log.Info("installed the node units", "root", *root)
			return 0
		case "start":
			if err := nodeagent.StartUnits(commandContext(), cfg, log); err != nil {
				return fail(command+" start", err)
			}
			return 0
		default:
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
	}
	if err := fs.Parse(args); err != nil {
		return flagCode(err)
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cfg, err := nodeagent.LoadConfig(*configPath)
	if err != nil {
		return fail(command, err)
	}
	ctx := commandContext()
	log := logger()
	switch command {
	case "join":
		err = nodeagent.Join(ctx, cfg, log)
	case "cni":
		err = nodeagent.CNI(ctx, cfg, log)
	case "diagnose":
		err = nodeagent.Diagnose(ctx, cfg, log)
	case "hostname":
		err = nodeagent.Hostname(ctx, cfg, log)
	case "dns-link":
		err = nodeagent.DNSLink(ctx, cfg, log)
	case "masquerade":
		err = nodeagent.Masquerade(ctx, cfg, log)
	case "install-meshd":
		err = nodeagent.InstallMeshd(ctx, cfg, log)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		return fail(command, err)
	}
	return 0
}

func configPathFromEnv() string {
	if path := os.Getenv("SOCIALWEB_NODE_CONFIG"); path != "" {
		return path
	}
	return defaultConfigPath
}

func logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func commandContext() context.Context {
	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	return ctx
}

func fail(command string, err error) int {
	fmt.Fprintf(os.Stderr, "socialweb-node %s: %v\n", command, err)
	return 1
}

func flagCode(err error) int {
	if err == flag.ErrHelp {
		return 0
	}
	return 2
}

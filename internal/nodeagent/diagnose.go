package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/clock"
)

type crictlCondition struct {
	Type string `json:"type"`

	Status any `json:"status"`

	Reason string `json:"reason"`

	Message string `json:"message"`
}

type crictlInfo struct {
	Status struct {
		Conditions []crictlCondition `json:"conditions"`
	} `json:"status"`

	LastCNILoadStatus any `json:"lastCNILoadStatus"`

	Config struct {
		CNI struct {
			ConfDir string `json:"confDir"`

			BinDirs []string `json:"binDirs"`

			BinDir string `json:"binDir"`
		} `json:"cni"`
	} `json:"config"`
}

func Diagnose(ctx context.Context, cfg Config, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	for attempt := 1; attempt <= cfg.DiagnoseAttempts; attempt++ {
		out, err := run(ctx, cfg.K3sPath, "crictl", "info")
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Warn("crictl info failed", "attempt", attempt, "of", cfg.DiagnoseAttempts, "err", err.Error())
			if attempt < cfg.DiagnoseAttempts {
				if err := clock.SleepErr(ctx, cfg.DiagnoseInterval()); err != nil {
					return err
				}
			}
			continue
		}
		var info crictlInfo
		if err := json.Unmarshal(out, &info); err != nil {
			log.Warn("crictl info did not return JSON", "attempt", attempt, "of", cfg.DiagnoseAttempts, "err", err.Error())
			if attempt < cfg.DiagnoseAttempts {
				if err := clock.SleepErr(ctx, cfg.DiagnoseInterval()); err != nil {
					return err
				}
			}
			continue
		}
		ready := "unknown"
		reason := ""
		for _, condition := range info.Status.Conditions {
			if condition.Type != "NetworkReady" {
				continue
			}
			ready = fmt.Sprint(condition.Status)
			reason = strings.TrimSpace(condition.Reason + " " + condition.Message)
			break
		}
		if ready == "true" {
			log.Info("NetworkReady=true")
			return nil
		}
		binDirs := info.Config.CNI.BinDirs
		if len(binDirs) == 0 && info.Config.CNI.BinDir != "" {
			binDirs = []string{info.Config.CNI.BinDir}
		}
		confDir := info.Config.CNI.ConfDir
		log.Info("the node network is not ready", "NetworkReady", ready, "reason", reason,
			"lastCNILoadStatus", fmt.Sprint(info.LastCNILoadStatus), "confDir", confDir,
			"confDirHolds", dirNames(confDir), "binDirs", strings.Join(binDirs, ","),
			"binDirsHold", binDirNames(binDirs), "attempt", attempt, "of", cfg.DiagnoseAttempts)
		if attempt < cfg.DiagnoseAttempts {
			if err := clock.SleepErr(ctx, cfg.DiagnoseInterval()); err != nil {
				return err
			}
		}
	}
	log.Info("the node network was still not ready", "readings", cfg.DiagnoseAttempts)
	return nil
}

func dirNames(dir string) string {
	if dir == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err.Error()
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}

func binDirNames(dirs []string) string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		out = append(out, dir+"="+dirNames(dir))
	}
	return strings.Join(out, " ")
}

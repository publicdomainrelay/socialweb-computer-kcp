package notify

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvSocket = "NOTIFY_SOCKET"

	EnvWatchdogUSec = "WATCHDOG_USEC"
)

func Ready() error { return send("READY=1") }

func Watchdog() error { return send("WATCHDOG=1") }

func send(state string) error {
	addr := os.Getenv(EnvSocket)
	if addr == "" {
		return nil
	}
	c, err := net.Dial("unixgram", socketName(addr))
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Write([]byte(state))
	return err
}

func socketName(addr string) string {
	if strings.HasPrefix(addr, "@") {
		return "\x00" + addr[1:]
	}
	return addr
}

func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(EnvWatchdogUSec)), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	return time.Duration(usec) * time.Microsecond
}

func KeepAlive(ctx context.Context, onError func(error)) {
	interval := WatchdogInterval()
	if interval <= 0 || os.Getenv(EnvSocket) == "" {
		return
	}
	tick := time.NewTicker(interval / 2)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := Watchdog(); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

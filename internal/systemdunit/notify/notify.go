package notify

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func Notify(message string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify: dialing %s: %w", socket, err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(strings.TrimRight(message, "\n") + "\n")); err != nil {
		return fmt.Errorf("notify: writing to %s: %w", socket, err)
	}
	return nil
}

func Ready(status string) error {
	message := "READY=1"
	if status != "" {
		message += "\nSTATUS=" + status
	}
	return Notify(message)
}

func Status(status string) error { return Notify("STATUS=" + status) }

func Stopping(status string) error {
	message := "STOPPING=1"
	if status != "" {
		message += "\nSTATUS=" + status
	}
	return Notify(message)
}

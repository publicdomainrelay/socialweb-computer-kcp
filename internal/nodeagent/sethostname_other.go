//go:build !linux

package nodeagent

import "fmt"

func setHostname(name string) error {
	return fmt.Errorf("nodeagent: setting the hostname to %s is supported only on linux", name)
}

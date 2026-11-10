//go:build !linux && !js

package deno

import "errors"

func ExecFenced([]string) error {
	return errors.New("sandbox: the IPv6 fence of a public-net deno program runs on linux only")
}

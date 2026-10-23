//go:build !linux

package main

import "time"

func awaitExit(pids []int, timeout time.Duration) {}

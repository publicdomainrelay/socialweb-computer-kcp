//go:build !linux

package marketnetwork

import "time"

func exits([]int) []int { return nil }

func awaitExits([]int, time.Duration) {}

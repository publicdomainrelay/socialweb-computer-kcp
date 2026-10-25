//go:build js

package portscan

func listeners() (map[int][]int, error) { return map[int][]int{}, nil }

func datagramListeners() (map[int][]int, error) { return map[int][]int{}, nil }

func cmdline(int) ([]string, error) { return nil, nil }

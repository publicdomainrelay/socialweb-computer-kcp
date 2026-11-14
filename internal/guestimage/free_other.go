//go:build !unix

package guestimage

func freeBytes(string) (int64, bool) { return 0, false }

//go:build js

package hostos

import "os"

const (
	LockShared = 1

	LockExclusive = 2

	LockNonBlocking = 4
)

func Lock(*os.File, int) error { return nil }

func Unlock(*os.File) error { return nil }

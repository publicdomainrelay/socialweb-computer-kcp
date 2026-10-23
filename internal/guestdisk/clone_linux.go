//go:build linux

package guestdisk

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const copyChunk = 1 << 20

func cloneInto(out, in *os.File) error {
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err == nil {
		return nil
	}
	info, err := in.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	for offset := int64(0); offset < size; {
		data, err := unix.Seek(int(in.Fd()), offset, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break
		}
		if err != nil {
			if _, err := in.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				return err
			}
			return out.Truncate(size)
		}
		hole, err := unix.Seek(int(in.Fd()), data, unix.SEEK_HOLE)
		if err != nil {
			hole = size
		}
		if err := copyExtent(out, in, data, hole-data); err != nil {
			return err
		}
		offset = hole
	}
	return out.Truncate(size)
}

func copyExtent(out, in *os.File, offset, length int64) error {
	buf := make([]byte, copyChunk)
	for done := int64(0); done < length; {
		want := min(length-done, copyChunk)
		n, err := in.ReadAt(buf[:want], offset+done)
		if n > 0 {
			if _, writeErr := out.WriteAt(buf[:n], offset+done); writeErr != nil {
				return writeErr
			}
			done += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}

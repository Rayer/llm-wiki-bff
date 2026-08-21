//go:build darwin || linux

package queryconfig

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSecureQueryConfig(path string) (*os.File, error) {
	// O_NONBLOCK keeps special files such as FIFOs from blocking before fstat.
	// O_NOFOLLOW closes the pathname symlink race; all subsequent checks and reads
	// use this same descriptor.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "")
	if file == nil {
		_ = unix.Close(fd)
		return nil, os.ErrInvalid
	}
	return file, nil
}

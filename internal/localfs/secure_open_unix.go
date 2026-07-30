//go:build darwin || linux

package localfs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const secureOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW

func openScopedRegularFile(root, userID, projectID, relPath string) (*os.File, error) {
	rootFD, err := unix.Open(root, secureOpenFlags|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	currentFD := rootFD
	components := []string{"users", userID, "projects", projectID}
	components = append(components, strings.Split(filepath.ToSlash(relPath), "/")...)
	for i, component := range components {
		flags := secureOpenFlags
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(currentFD, component, flags, 0)
		if err != nil {
			_ = unix.Close(currentFD)
			return nil, &os.PathError{Op: "openat", Path: relPath, Err: err}
		}
		if err := unix.Close(currentFD); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		currentFD = fd
	}

	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		_ = unix.Close(currentFD)
		return nil, &os.PathError{Op: "fstat", Path: relPath, Err: err}
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(currentFD)
		return nil, fmt.Errorf("%w: %s", errSecureNotRegular, relPath)
	}
	return os.NewFile(uintptr(currentFD), relPath), nil
}

//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

var errSnapshotPathNotFound = errors.New("snapshot path not found")

func openSnapshotRegularFile(root, relPath string) (*os.File, error) {
	components, err := snapshotPathComponents(relPath)
	if err != nil {
		return nil, err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, errSnapshotPathNotFound
		}
		return nil, &os.PathError{Op: "open", Path: relPath, Err: err}
	}
	currentFD := rootFD
	for i, component := range components {
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
		if i < len(components)-1 {
			flags |= unix.O_DIRECTORY
		}
		fd, openErr := unix.Openat(currentFD, component, flags, 0)
		_ = unix.Close(currentFD)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return nil, errSnapshotPathNotFound
			}
			return nil, &os.PathError{Op: "openat", Path: relPath, Err: openErr}
		}
		currentFD = fd
	}
	var stat unix.Stat_t
	if err := unix.Fstat(currentFD, &stat); err != nil {
		_ = unix.Close(currentFD)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(currentFD)
		return nil, fmt.Errorf("snapshot path is not a regular file: %s", relPath)
	}
	return os.NewFile(uintptr(currentFD), relPath), nil
}

func snapshotPathComponents(relPath string) ([]string, error) {
	if relPath == "" || strings.Contains(relPath, "\\") || path.IsAbs(relPath) || path.Clean(relPath) != relPath {
		return nil, errors.New("snapshot path must be clean and relative")
	}
	components := strings.Split(relPath, "/")
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return nil, errors.New("snapshot path must be clean and relative")
		}
	}
	return components, nil
}

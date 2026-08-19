//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

var errSnapshotPathNotFound = errors.New("snapshot path not found")

func openSnapshotRegularFile(string, string) (*os.File, error) {
	return nil, errors.New("snapshot reader is unsupported on this platform")
}

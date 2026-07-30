//go:build !darwin && !linux

package localfs

import (
	"errors"
	"os"
)

func openScopedRegularFile(_, _, _, _ string) (*os.File, error) {
	return nil, errors.New("secure local file access is unsupported on this platform")
}

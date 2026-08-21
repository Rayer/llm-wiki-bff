//go:build !darwin && !linux

package queryconfig

import (
	"os"
)

func openSecureQueryConfig(string) (*os.File, error) {
	return nil, errSecureQueryConfigUnsupported
}

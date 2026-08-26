//go:build !unix

package safefile

import (
	"errors"
	"os"
)

var errPlatformUnsupported = errors.New("unsupported platform")

func openTrustedRegular(string) (*os.File, error) {
	return nil, errPlatformUnsupported
}

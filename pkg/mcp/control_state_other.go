//go:build !unix

package mcp

import (
	"context"
	"errors"
	"os"
)

var errControlStateUnsupported = errors.New("operator control state is supported only on Unix")

func validateControlDirectory(string) error { return errControlStateUnsupported }

func lockControlState(context.Context, string) (func(), error) {
	return nil, errControlStateUnsupported
}

func readControlFile(string, int64) ([]byte, error) {
	return nil, errControlStateUnsupported
}

func writeControlFileAtomic(string, []byte, os.FileMode) error {
	return errControlStateUnsupported
}

func syncControlDirectory(string) error { return errControlStateUnsupported }

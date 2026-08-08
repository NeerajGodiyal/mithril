//go:build !unix

package controlaudit

import (
	"errors"
	"os"
)

func createStoreObject(string) (*os.File, error) {
	return nil, errors.New("secure audit stores are unsupported on this platform")
}

func openExistingStoreObject(string, bool) (*os.File, error) {
	return nil, errors.New("secure audit stores are unsupported on this platform")
}

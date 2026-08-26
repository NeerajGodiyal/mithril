//go:build !unix

package safefile

import "os"

// OwnerTrusted fails closed where this implementation cannot verify file
// ownership. Callers handle the false result as a configuration error.
func OwnerTrusted(os.FileInfo) bool {
	return false
}

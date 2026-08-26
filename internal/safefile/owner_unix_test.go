//go:build unix

package safefile

import "testing"

func TestOwnerAllowed(t *testing.T) {
	if !ownerAllowed(0, 1000) || !ownerAllowed(1000, 1000) {
		t.Fatal("root or process-owned file was rejected")
	}
	if ownerAllowed(1001, 1000) || ownerAllowed(1000, 0) {
		t.Fatal("file controlled by another user was accepted")
	}
}

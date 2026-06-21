package tpu

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
)

// LoadIdentity reads a Solana validator identity keypair JSON file.
func LoadIdentity(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, fmt.Errorf("identity path is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity %q: %w", path, err)
	}
	var keypair []byte
	if err := json.Unmarshal(raw, &keypair); err != nil {
		return nil, fmt.Errorf("parse identity %q: %w", path, err)
	}
	if len(keypair) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("identity %q: expected %d-byte keypair, got %d", path, ed25519.PrivateKeySize, len(keypair))
	}
	return append(ed25519.PrivateKey(nil), keypair...), nil
}

package accounts

import (
	"testing"
	"time"
)

func TestMemAccountsReadsAreConcurrent(t *testing.T) {
	mem := NewMemAccounts()
	var key [32]byte
	key[0] = 1
	if err := mem.SetAccount(&key, &Account{Key: key}); err != nil {
		t.Fatalf("set account: %v", err)
	}

	// Hold one read lock while GetAccount acquires another. This models
	// parallel transactions falling through an overlay to an immutable parent.
	mem.mu.RLock()
	done := make(chan error, 1)
	go func() {
		_, err := mem.GetAccount(&key)
		done <- err
	}()

	select {
	case err := <-done:
		mem.mu.RUnlock()
		if err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
	case <-time.After(time.Second):
		mem.mu.RUnlock()
		t.Fatal("account read serialized behind another reader")
	}
}

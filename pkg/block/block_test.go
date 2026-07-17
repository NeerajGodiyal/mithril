package block

import (
	"encoding/json"
	"testing"
)

func TestTransactionSignaturesVerifiedMarkerIsNotSerialized(t *testing.T) {
	original := &Block{Slot: 42}
	original.MarkTransactionSignaturesVerified()
	if !original.TransactionSignaturesVerified() {
		t.Fatal("verification marker was not set")
	}

	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}

	var decoded Block
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal block: %v", err)
	}
	if decoded.TransactionSignaturesVerified() {
		t.Fatal("verification marker crossed a serialization boundary")
	}
}

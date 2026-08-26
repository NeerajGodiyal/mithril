package replay

import (
	"encoding/hex"
	"testing"
)

func TestGenerateRunIDUses128Bits(t *testing.T) {
	runID := GenerateRunID()
	decoded, err := hex.DecodeString(runID)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != runID {
		t.Fatalf("GenerateRunID() returned invalid lineage ID %q", runID)
	}
}

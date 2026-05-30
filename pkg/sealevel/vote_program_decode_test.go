package sealevel

import (
	"encoding/binary"
	"testing"

	bin "github.com/gagliardetto/binary"
)

func TestVoteInstrUpdateVoteStateRejectsHugeLockoutCount(t *testing.T) {
	raw := make([]byte, 8)
	binary.LittleEndian.PutUint64(raw, ^uint64(0))
	decoder := bin.NewBinDecoder(raw)

	var vote VoteInstrUpdateVoteState
	if err := vote.UnmarshalWithDecoder(decoder); err == nil {
		t.Fatalf("expected huge lockout count to fail")
	}
}

func TestVoteInstrTowerSyncRejectsHugeLockoutCount(t *testing.T) {
	raw := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
		0xff, 0xff, 0x03,
	}
	decoder := bin.NewBinDecoder(raw)

	var vote VoteInstrTowerSync
	if err := vote.UnmarshalWithDecoder(decoder); err == nil {
		t.Fatalf("expected huge lockout count to fail")
	}
}

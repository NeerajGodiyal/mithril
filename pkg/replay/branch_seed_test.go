package replay

import (
	"encoding/base64"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
)

// Seed fidelity: a branch's end-of-slot sysvar state, carried through the resume
// context (the fork coordinator's per-branch snapshot), must survive the
// encode→seed→read round trip exactly. Any loss here would silently corrupt the
// sysvars a forked block executes against.
func TestBranchSysvarSeedRoundTrip(t *testing.T) {
	rbh := sealevel.SysvarRecentBlockhashes{
		{Blockhash: [32]byte{0xAA, 1}, FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000}},
		{Blockhash: [32]byte{0xBB, 2}, FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 10000}},
	}
	slotHashes := sealevel.SysvarSlotHashes{
		{Slot: 900, Hash: [32]byte{0xCC}},
		{Slot: 899, Hash: [32]byte{0xDD}},
	}
	clock := sealevel.SysvarClock{
		Slot: 901, EpochStartTimestamp: 111, Epoch: 7, LeaderScheduleEpoch: 8, UnixTimestamp: 222,
	}

	ctx := &state.ResumeContext{
		RecentBlockhashes: EncodeRecentBlockhashes(&rbh),
		SlotHashes:        EncodeSlotHashes(&slotHashes),
		Clock:             base64.StdEncoding.EncodeToString(clock.MustMarshal()),
	}

	slotCtx := &sealevel.SlotCtx{}
	if err := seedBranchSysvarsFromContext(slotCtx, ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	execCtx := &sealevel.ExecutionCtx{SlotCtx: slotCtx}
	gotClock, err := sealevel.ReadClockSysvar(execCtx)
	if err != nil {
		t.Fatalf("read clock: %v", err)
	}
	if gotClock != clock {
		t.Fatalf("clock round trip mismatch: got %+v want %+v", gotClock, clock)
	}

	gotRBH, err := sealevel.ReadRecentBlockHashesSysvar(execCtx)
	if err != nil {
		t.Fatalf("read rbh: %v", err)
	}
	if len(gotRBH) != len(rbh) || gotRBH[0] != rbh[0] || gotRBH[1] != rbh[1] {
		t.Fatalf("recent blockhashes round trip mismatch: got %+v want %+v", gotRBH, rbh)
	}

	gotSH, err := sealevel.ReadSlotHashesSysvar(execCtx)
	if err != nil {
		t.Fatalf("read slot hashes: %v", err)
	}
	if len(gotSH) != len(slotHashes) || gotSH[0] != slotHashes[0] || gotSH[1] != slotHashes[1] {
		t.Fatalf("slot hashes round trip mismatch: got %+v want %+v", gotSH, slotHashes)
	}
}

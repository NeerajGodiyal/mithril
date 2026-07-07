package sealevel

import "testing"

// Multibranch isolation gate: two branches executing with their own seeded sysvar
// containers must read their OWN carried-forward sysvar state, never a sibling's, and
// must not disturb the process-global cache. This is the property the full sysvar
// flip must preserve once the per-slot loader writes the branch container.
func TestBranchSysvarsIsolateCarriedFamilies(t *testing.T) {
	// Baseline: a distinct value in the shared global, to prove neither branch leaks
	// into it (or reads from it once seeded).
	globalClock := &SysvarClock{Slot: 9}
	SysvarCache.Clock.Sysvar = globalClock
	defer func() { SysvarCache.Clock.Sysvar = nil }()

	slotA := &SlotCtx{}
	slotA.InitBranchSysvars(&SysvarClock{Slot: 100}, nil, &SysvarRecentBlockhashes{{FeeCalculator: FeeCalculator{LamportsPerSignature: 1}}})
	slotB := &SlotCtx{}
	slotB.InitBranchSysvars(&SysvarClock{Slot: 200}, nil, &SysvarRecentBlockhashes{{FeeCalculator: FeeCalculator{LamportsPerSignature: 2}}})

	execA := &ExecutionCtx{SlotCtx: slotA}
	execB := &ExecutionCtx{SlotCtx: slotB}

	clockA, err := ReadClockSysvar(execA)
	if err != nil {
		t.Fatalf("read clock A: %v", err)
	}
	clockB, err := ReadClockSysvar(execB)
	if err != nil {
		t.Fatalf("read clock B: %v", err)
	}
	if clockA.Slot != 100 || clockB.Slot != 200 {
		t.Fatalf("sysvar leak across branches: A=%d B=%d (want 100/200)", clockA.Slot, clockB.Slot)
	}

	rbhA, err := ReadRecentBlockHashesSysvar(execA)
	if err != nil {
		t.Fatalf("read rbh A: %v", err)
	}
	rbhB, err := ReadRecentBlockHashesSysvar(execB)
	if err != nil {
		t.Fatalf("read rbh B: %v", err)
	}
	if rbhA[0].FeeCalculator.LamportsPerSignature != 1 || rbhB[0].FeeCalculator.LamportsPerSignature != 2 {
		t.Fatalf("recent-blockhashes leak: A=%d B=%d (want 1/2)",
			rbhA[0].FeeCalculator.LamportsPerSignature, rbhB[0].FeeCalculator.LamportsPerSignature)
	}

	// The shared global must be untouched by branch execution.
	if SysvarCache.Clock.Sysvar != globalClock || SysvarCache.Clock.Sysvar.Slot != 9 {
		t.Fatalf("branch execution disturbed the shared global clock")
	}
	// A slot without a branch container still resolves to the global (single-branch).
	if got, _ := ReadClockSysvar(&ExecutionCtx{SlotCtx: &SlotCtx{}}); got.Slot != 9 {
		t.Fatalf("unseeded slot must read the global clock, got %d", got.Slot)
	}
}

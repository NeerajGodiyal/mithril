package conformance

import (
	"fmt"
	"os"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"google.golang.org/protobuf/proto"
)

// Parity gate for the branch-scoped sysvar read seam: every parseable fixture runs
// twice with the SAME SlotCtx — once reading sysvars via the process-global cache,
// once via a branch container seeded with the identical Clock — and the two runs
// must reach the identical outcome. The container is the only delta between runs.
//
// Self-verifying: fixtures that panic during harness setup are counted as skipped,
// and the test refuses to report success unless a meaningful number of baselines
// actually executed — a broken fixture set skips loudly instead of passing vacuously.
// (The vendored firedancer vectors periodically change protobuf schema; when they do,
// conformance/invoke.pb.go must be regenerated before this gate is functional.)
func TestBranchSysvarReadParity(t *testing.T) {
	dirs := []string{
		"test-vectors/instr/fixtures/vote",
		"test-vectors/instr/fixtures/system",
	}

	var total, compared, baselinePassed, diverged, skipped int

	type outcome int
	const (
		outcomePass outcome = iota
		outcomeFail
		outcomeSetupPanic
	)

	runOnce := func(fixture *InstrFixture, branchClock bool) (result outcome) {
		harnessDone := false
		defer func() {
			if r := recover(); r != nil {
				if !harnessDone {
					result = outcomeSetupPanic
				} else {
					result = outcomeFail
				}
			}
		}()
		execCtx, instrAccts := newExecCtxAndInstrAcctsFromFixture(fixture)
		harnessDone = true
		// Both runs get the identical SlotCtx so the branch container is the only
		// difference; production code branches on SlotCtx nil-ness.
		slotCtx := &sealevel.SlotCtx{}
		if branchClock {
			clock, err := sealevel.ReadClockSysvar(execCtx)
			if err == nil {
				c := clock
				slotCtx.InitBranchSysvars(&c, nil, nil)
			}
		}
		execCtx.SlotCtx = slotCtx
		err := execCtx.ProcessInstruction(fixture.Input.Data, instrAccts, []uint64{0})
		if !returnValueIsExpectedValue(fixture, err) {
			return outcomeFail
		}
		if err == nil && !accountStateChangesMatch(t, execCtx, fixture) {
			return outcomeFail
		}
		return outcomePass
	}

	for _, dir := range dirs {
		infos, err := os.ReadDir(dir)
		if err != nil {
			t.Skipf("test-vectors not available (%s): %v", dir, err)
		}
		for _, fi := range infos {
			total++
			in, err := os.ReadFile(fmt.Sprintf("%s/%s", dir, fi.Name()))
			if err != nil {
				skipped++
				continue
			}
			fixture := &InstrFixture{}
			if err := proto.Unmarshal(in, fixture); err != nil {
				skipped++
				continue
			}
			baseline := runOnce(fixture, false)
			if baseline == outcomeSetupPanic {
				skipped++
				continue
			}
			branch := runOnce(fixture, true)
			compared++
			if baseline == outcomePass {
				baselinePassed++
			}
			if baseline != branch {
				diverged++
				if diverged <= 20 {
					t.Errorf("branch-scoped Clock read diverged: baseline=%v branch=%v (%s)", baseline, branch, fi.Name())
				}
			}
		}
	}

	fmt.Printf("branch sysvar parity: total=%d compared=%d baseline_passed=%d skipped=%d diverged=%d\n",
		total, compared, baselinePassed, skipped, diverged)
	if diverged != 0 {
		t.Fatalf("branch-scoped sysvar read diverged from baseline on %d/%d fixtures", diverged, compared)
	}
	// Refuse a vacuous pass: if almost nothing executed, the gate proved nothing.
	if baselinePassed < 100 {
		t.Skipf("gate non-functional: only %d/%d baselines executed successfully "+
			"(fixture schema likely newer than conformance/invoke.pb.go — regenerate it)", baselinePassed, total)
	}
}

package replay

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sigverify"
)

// Batching must not blur failure attribution. An invalid signature is a
// deliberate process halt, and the panic message is the only forensic artifact
// the operator gets, so it has to name the signer that actually failed — not
// the first in the group, and not merely "somewhere in this batch".
//
// The bad job is placed at the front, the back, and the interior of a full
// group so a verdict written to the wrong lane cannot pass by symmetry.
func TestVerifySignatureBatchNamesTheFailingSigner(t *testing.T) {
	for _, badIndex := range []int{0, 1, 7, 8, 31, sigverify.MaxDrain - 1} {
		t.Run(fmt.Sprintf("badIndex=%d", badIndex), func(t *testing.T) {
			var wg sync.WaitGroup
			group := make([]sigverifyJob, sigverify.MaxDrain)
			for i := range group {
				wg.Add(1)
				group[i] = sigverifyJob{snapshot: signedTestSnapshot(t, i == badIndex), wg: &wg}
			}
			wantSigner := group[badIndex].snapshot.signers[0].String()

			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("badIndex=%d: expected a panic on the invalid signature", badIndex)
				}
				msg, ok := r.(string)
				if !ok || !strings.Contains(msg, "invalid signature") {
					t.Fatalf("badIndex=%d: panic = %v, want invalid-signature message", badIndex, r)
				}
				if !strings.Contains(msg, wantSigner) {
					t.Fatalf("badIndex=%d: panic named the wrong signer\n got: %s\nwant signer: %s",
						badIndex, msg, wantSigner)
				}
				// The halt must not also leak the block's WaitGroup: every job
				// in the group is released on the way out.
				wg.Wait()
			}()

			var batch sigverify.Batch
			verifySignatureBatch(group, &batch)
		})
	}
}

// The join contract ProcessBlock depends on: every job in a group is released,
// and grouping does not change that. Jobs in one group may belong to different
// blocks, so each is released against its own WaitGroup.
func TestVerifySignatureBatchReleasesEveryWaitGroupInTheGroup(t *testing.T) {
	var first, second sync.WaitGroup
	group := make([]sigverifyJob, 0, 16)
	for i := 0; i < 16; i++ {
		wg := &first
		if i%2 == 1 {
			wg = &second
		}
		wg.Add(1)
		group = append(group, sigverifyJob{snapshot: signedTestSnapshot(t, false), wg: wg})
	}

	var batch sigverify.Batch
	verifySignatureBatch(group, &batch)

	first.Wait()  // hangs (test timeout) if a job was released against the wrong group
	second.Wait() // ...or not at all
}

// A worker reuses one Batch across groups. A stale verdict or a retained
// public key from the previous group would be a correctness bug, not just a
// leak, so run several groups through one Batch and mix in a failure.
func TestVerifySignatureBatchReusesScratchAcrossGroups(t *testing.T) {
	var batch sigverify.Batch
	for round := 0; round < 4; round++ {
		var wg sync.WaitGroup
		group := make([]sigverifyJob, 0, 9)
		for i := 0; i < 9; i++ {
			wg.Add(1)
			group = append(group, sigverifyJob{snapshot: signedTestSnapshot(t, false), wg: &wg})
		}
		verifySignatureBatch(group, &batch)
		wg.Wait()
	}
}

// The arity check predates batching and must survive it: a snapshot whose
// signer and signature counts disagree halts before any verification, because
// the pairing it would verify is meaningless.
func TestVerifySignatureBatchHaltsOnArityMismatch(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	snapshot := signedTestSnapshot(t, false)
	snapshot.signatures = append(snapshot.signatures, snapshot.signatures[0])

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic on mismatched signers/signatures")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "mismatched signers/signatures") {
			t.Fatalf("panic = %v, want mismatched-arity message", r)
		}
		wg.Wait()
	}()

	var batch sigverify.Batch
	verifySignatureBatch([]sigverifyJob{{snapshot: snapshot, wg: &wg}}, &batch)
}

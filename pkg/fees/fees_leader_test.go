package fees

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// staleTailReader fakes the speculative-state read: it holds the leader's LATEST
// balance (including unrooted fee credits), while the durable store would be stale.
type staleTailReader struct {
	acct *accounts.Account
}

func (r *staleTailReader) GetAccount(slot uint64, pk solana.PublicKey) (*accounts.Account, error) {
	return r.acct.Clone(), nil
}

// Regression for the rooted-durable divergence (slot 430276043): when the leader
// is absent from the block, its balance MUST come from the speculative-state read
// (UnrootedRead), not a direct disk read that misses unrooted fee credits.
func TestDistributeTxFeesUsesSpeculativeRead(t *testing.T) {
	leader := solana.PublicKey{0xAB}
	latest := &accounts.Account{Key: leader, Lamports: 10_000_000} // includes unrooted credits

	slotCtx := &sealevel.SlotCtx{
		Slot:         42,
		Accounts:     accounts.NewMemAccounts(), // leader NOT in the block
		ParentAccts:  accounts.NewMemAccounts(),
		UnrootedRead: &staleTailReader{acct: latest},
		Features:     features.NewFeaturesDefault(),
	}

	acc := TxFeeInfoAccumulator{TotalFees: 10_000}
	DistributeTxFeesToSlotLeader(nil, slotCtx, leader, &acc) // nil acctsDb: MUST not be touched

	got, err := slotCtx.GetAccount(leader)
	if err != nil {
		t.Fatalf("leader must be set on slotCtx: %v", err)
	}
	wantFees := uint64(10_000 - 10_000/2) // non-full-priority split: total - burn(half)
	if got.Lamports != 10_000_000+wantFees {
		t.Fatalf("leader balance = %d, want latest(10000000) + fees(%d) — stale base means the speculative read was bypassed", got.Lamports, wantFees)
	}
}

// sharedTailReader returns the SAME object on every read, as the real accountsdb
// read caches do (VoteAcctCache/CommonAcctsCache return stored pointers).
type sharedTailReader struct {
	acct *accounts.Account
}

func (r *sharedTailReader) GetAccount(slot uint64, pk solana.PublicKey) (*accounts.Account, error) {
	return r.acct, nil
}

// Regression for the branch-differential divergence (alpenglow slot 5917835): the
// fallback leader read resolves to a SHARED cache object; crediting fees in place
// poisons it, so a sibling-fork or shadow re-execution reading the same object
// double-credits the leader. The source object must never be mutated.
func TestDistributeTxFeesDoesNotMutateSharedRead(t *testing.T) {
	leader := solana.PublicKey{0xAC}
	shared := &accounts.Account{Key: leader, Lamports: 5_150_492_500}
	reader := &sharedTailReader{acct: shared}
	acc := TxFeeInfoAccumulator{TotalFees: 3_375_000}
	const want = 5_150_492_500 + 1_687_500 // base + (total - burn half)

	runOnce := func() uint64 {
		slotCtx := &sealevel.SlotCtx{
			Slot:         5917835,
			Accounts:     accounts.NewMemAccounts(), // leader NOT in the block
			ParentAccts:  accounts.NewMemAccounts(),
			UnrootedRead: reader,
			Features:     features.NewFeaturesDefault(),
		}
		DistributeTxFeesToSlotLeader(nil, slotCtx, leader, &acc)
		got, err := slotCtx.GetAccount(leader)
		if err != nil {
			t.Fatalf("leader must be set on slotCtx: %v", err)
		}
		return got.Lamports
	}

	if got := runOnce(); got != want {
		t.Fatalf("first execution: leader = %d, want %d", got, want)
	}
	if shared.Lamports != 5_150_492_500 {
		t.Fatalf("shared cache object mutated in place (now %d): sibling forks reading it double-credit", shared.Lamports)
	}
	// A sibling branch / shadow re-execution from the same parent state must
	// produce the identical result.
	if got := runOnce(); got != want {
		t.Fatalf("second execution from same parent state: leader = %d, want %d (double credit)", got, want)
	}
}

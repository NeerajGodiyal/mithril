package bankhash

import (
	"bytes"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

func acctWith(b byte, lamports uint64) *accounts.Account {
	var key solana.PublicKey
	key[0] = b
	return &accounts.Account{Key: key, Lamports: lamports}
}

func keysOf(accts []*accounts.Account) []byte {
	out := make([]byte, len(accts))
	for i, a := range accts {
		out[i] = a.Key[0]
	}
	return out
}

func TestDedupeModifiedAccts(t *testing.T) {
	t.Run("nil and short inputs pass through", func(t *testing.T) {
		if got := dedupeModifiedAccts(nil); got != nil {
			t.Fatalf("nil input: got %v", got)
		}
		one := acctWith(1, 100)
		got := dedupeModifiedAccts([]*accounts.Account{one})
		if len(got) != 1 || got[0] != one {
			t.Fatalf("single input not preserved: %v", got)
		}
	})

	t.Run("no duplicates preserves order and identity", func(t *testing.T) {
		in := []*accounts.Account{acctWith(1, 10), acctWith(2, 20), acctWith(3, 30)}
		got := dedupeModifiedAccts(in)
		if string(keysOf(got)) != string([]byte{1, 2, 3}) {
			t.Fatalf("order changed: %v", keysOf(got))
		}
	})

	t.Run("duplicate keeps last value at first-seen position", func(t *testing.T) {
		first := acctWith(1, 10)
		mid := acctWith(2, 20)
		last := acctWith(1, 99) // same key as first, newer value
		got := dedupeModifiedAccts([]*accounts.Account{first, mid, last})
		if len(got) != 2 {
			t.Fatalf("expected 2 unique, got %d", len(got))
		}
		if string(keysOf(got)) != string([]byte{1, 2}) {
			t.Fatalf("first-seen order not preserved: %v", keysOf(got))
		}
		if got[0].Lamports != 99 {
			t.Fatalf("expected newest value (99) for key 1, got %d", got[0].Lamports)
		}
	})

	t.Run("nil entries dropped", func(t *testing.T) {
		got := dedupeModifiedAccts([]*accounts.Account{acctWith(1, 10), nil, acctWith(2, 20)})
		if len(got) != 2 {
			t.Fatalf("expected 2 after dropping nil, got %d", len(got))
		}
	})
}

// ctxWithParent builds a minimal SlotCtx whose ParentAccts holds `old` under `key`,
// exercising the real calculateDeltaLtHash → calculateSingleDeltaLtHash → GetParentAccount path.
func ctxWithParent(t *testing.T, key solana.PublicKey, old *accounts.Account) *sealevel.SlotCtx {
	t.Helper()
	parent := accounts.NewMemAccounts()
	if err := parent.SetAccountWithoutLock(key, old); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	return &sealevel.SlotCtx{Slot: 42, ParentAccts: parent, AcctsLtHash: &lthash.LtHash{}}
}

// PROOF that the dedupe is not cosmetic: a duplicate CHANGED account, without dedupe,
// double-counts its LtHash delta (the bug). With dedupe it counts exactly once.
func TestDedupePreventsLtHashDoubleCount(t *testing.T) {
	var key solana.PublicKey
	key[0] = 7

	old := &accounts.Account{Key: key, Lamports: 100, Data: []byte{1, 2, 3}, Owner: [32]byte{9}}
	changed := &accounts.Account{Key: key, Lamports: 250, Data: []byte{4, 5, 6}, Owner: [32]byte{9}}

	// The correct delta: the account modified once.
	single := calculateDeltaLtHash(ctxWithParent(t, key, old), []*accounts.Account{changed})

	// Same account appearing twice → dedupe must collapse to the single delta.
	deduped := calculateDeltaLtHash(ctxWithParent(t, key, old), []*accounts.Account{changed, changed})
	if !bytes.Equal(single.Hash(), deduped.Hash()) {
		t.Fatal("dedupe did not collapse the duplicate to a single delta")
	}

	// Applying the delta twice is the double-count this guards against.
	var doubled lthash.LtHash
	doubled.Add(single)
	doubled.Add(single)
	if bytes.Equal(doubled.Hash(), single.Hash()) {
		t.Fatal("setup bug: delta is zero, cannot demonstrate a double-count")
	}
	// Deduped must equal the single delta, not the double-counted value.
	if bytes.Equal(deduped.Hash(), doubled.Hash()) {
		t.Fatal("dedupe failed: produced the double-counted delta")
	}
}

// PROOF of keep-LAST at the real LtHash level: when a key appears twice with different
// values, the delta must reflect the LAST (newest) value, not the first (stale) one.
func TestDedupeKeepsLastValueInDelta(t *testing.T) {
	var key solana.PublicKey
	key[0] = 3

	old := &accounts.Account{Key: key, Lamports: 100, Data: []byte{1, 1, 1}, Owner: [32]byte{2}}
	stale := &accounts.Account{Key: key, Lamports: 150, Data: []byte{5, 5, 5}, Owner: [32]byte{2}}
	newest := &accounts.Account{Key: key, Lamports: 999, Data: []byte{8, 8, 8}, Owner: [32]byte{2}}

	wantNewest := calculateDeltaLtHash(ctxWithParent(t, key, old), []*accounts.Account{newest})
	wantStale := calculateDeltaLtHash(ctxWithParent(t, key, old), []*accounts.Account{stale})

	got := calculateDeltaLtHash(ctxWithParent(t, key, old), []*accounts.Account{stale, newest})
	if !bytes.Equal(got.Hash(), wantNewest.Hash()) {
		t.Fatal("dedupe delta did not match the newest value")
	}
	if bytes.Equal(got.Hash(), wantStale.Hash()) {
		t.Fatal("dedupe delta matched the STALE value (keep-first bug)")
	}
}

// PROOF the no-op reasoning holds: a duplicate of an UNCHANGED account (equal to parent)
// contributes zero delta whether deduped or not — so dedupe cannot alter a matched slot
// that only had unchanged-account duplicates.
func TestUnchangedDuplicateIsZeroDeltaEitherWay(t *testing.T) {
	var key solana.PublicKey
	key[0] = 11
	acct := &accounts.Account{Key: key, Lamports: 500, Data: []byte{7, 7}, Owner: [32]byte{4}}
	// parent == the same value → acctsEqual short-circuits to zero delta.
	same := &accounts.Account{Key: key, Lamports: 500, Data: []byte{7, 7}, Owner: [32]byte{4}}

	delta := calculateDeltaLtHash(ctxWithParent(t, key, same), []*accounts.Account{acct, acct})
	var zero lthash.LtHash
	if !bytes.Equal(delta.Hash(), zero.Hash()) {
		t.Fatal("unchanged duplicate produced a non-zero delta")
	}
}

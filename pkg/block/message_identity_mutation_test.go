package block

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
)

func TestTransactionMessageIdentityInvalidatesOnRecentBlockhashChange(t *testing.T) {
	tx := identityTestTransaction(1)
	block := &Block{Transactions: []*solana.Transaction{tx}}
	if _, err := block.PrepareTransactionMessageIdentities(); err != nil {
		t.Fatalf("prepare original identity: %v", err)
	}
	originalCache := block.transactionDerivedState.messageIdentities
	block.MarkTransactionSignaturesVerified()

	tx.Message.RecentBlockhash = solana.Hash{0x99}
	got, err := block.PrepareTransactionMessageIdentities()
	if err != nil {
		t.Fatalf("rebuild identity: %v", err)
	}
	if block.transactionDerivedState.messageIdentities == originalCache {
		t.Fatal("recent-blockhash mutation reused stale identity cache")
	}
	if block.TransactionSignaturesVerified() {
		t.Fatal("recent-blockhash mutation retained signature-verification trust")
	}
	want, err := txstatus.IdentityForTransaction(tx)
	if err != nil {
		t.Fatalf("hash mutated transaction: %v", err)
	}
	if got.Identity(0) != want {
		t.Fatalf("rebuilt identity = %+v, want %+v", got.Identity(0), want)
	}
}

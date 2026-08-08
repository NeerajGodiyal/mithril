package replay

import (
	"errors"
	"testing"
	"time"

	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
)

func submitReceipt(index *txstatus.Index, signature solana.Signature, recentBlockhash solana.Hash, deadline *uint64) {
	index.SubmissionAttempted(signature, recentBlockhash, deadline)
	index.Forwarded(signature)
}

func TestPublishSubmittedTransactionOutcomesUsesCommittedBlockEvidence(t *testing.T) {
	index, err := txstatus.NewIndex(txstatus.Config{MaxReceipts: 8, Retention: time.Hour})
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	success := solana.Signature{1}
	failed := solana.Signature{2}
	pending := solana.Signature{3}
	foreign := solana.Signature{4}
	deadline := uint64(250)
	for _, signature := range []solana.Signature{success, failed, pending} {
		submitReceipt(index, signature, solana.Hash{9}, &deadline)
	}

	block := &b.Block{
		Slot:        200,
		BlockHeight: 251,
		Transactions: []*solana.Transaction{
			{Signatures: []solana.Signature{success}},
			{Signatures: []solana.Signature{failed}},
			{Signatures: []solana.Signature{foreign}},
		},
	}
	publishSubmittedTransactionOutcomes(index, block, []error{nil, errors.New("InstructionError"), nil})

	if receipt, _ := index.Lookup(success); receipt.Status != txstatus.StatusLanded {
		t.Fatalf("successful receipt status = %v", receipt.Status)
	}
	if receipt, _ := index.Lookup(failed); receipt.Status != txstatus.StatusLandedFailed {
		t.Fatalf("failed receipt status = %v", receipt.Status)
	}
	if receipt, _ := index.Lookup(pending); receipt.Status != txstatus.StatusExpired {
		t.Fatalf("unseen receipt status = %v", receipt.Status)
	}
	if _, known := index.Lookup(foreign); known {
		t.Fatal("foreign transaction was added to the submitted-only index")
	}
}

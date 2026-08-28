package replay

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/addresses"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
)

func submitReceipt(t *testing.T, index *txstatus.Index, signature solana.Signature, recentBlockhash solana.Hash, deadline *uint64) {
	t.Helper()
	if err := index.SubmissionAttempted(signature, recentBlockhash, deadline); err != nil {
		t.Fatalf("SubmissionAttempted: %v", err)
	}
	index.Forwarded(signature)
}

func TestFollowerOutcomeUsesCanonicalTransactionFailure(t *testing.T) {
	slotCtx, cleanup := newCommitTestSlotCtx()
	defer cleanup()
	payer := txfixture.PayerPubkey()
	tx, err := solana.NewTransaction(
		[]solana.Instruction{solana.NewInstruction(
			addresses.ComputeBudgetProgramAddr,
			nil,
			[]byte{sealevel.ComputeBudgetInstrTypeSetComputeUnitLimit},
		)},
		txfixture.TestBlockhash(),
		solana.TransactionPayer(payer),
	)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := txfixture.PayerPrivateKey()
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer {
			return &privateKey
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var outcome string
	_, _, processingErr := processTransactionForReplay(
		slotCtx, &sync.WaitGroup{}, tx, nil, nil, nil, nil, &outcome, false,
	)
	if !errors.Is(processingErr, sealevel.InstrErrInvalidInstructionData) {
		t.Fatalf("processing error = %v", processingErr)
	}
	if outcome != "InstructionError(0, InvalidInstructionData)" {
		t.Fatalf("canonical outcome = %q", outcome)
	}
}

type receiptObservingSlotCtxSetter struct {
	index                *txstatus.Index
	success              solana.Signature
	pending              solana.Signature
	published            *sealevel.SlotCtx
	successAtPublication txstatus.Status
	pendingAtPublication txstatus.Status
}

func (setter *receiptObservingSlotCtxSetter) SetSlotCtx(slotCtx *sealevel.SlotCtx) {
	setter.published = slotCtx
	setter.successAtPublication, _ = lookupReceiptStatus(setter.index, setter.success)
	setter.pendingAtPublication, _ = lookupReceiptStatus(setter.index, setter.pending)
}

func lookupReceiptStatus(index *txstatus.Index, signature solana.Signature) (txstatus.Status, bool) {
	receipt, ok := index.Lookup(signature)
	return receipt.Status, ok
}

func TestSubmittedTransactionOutcomesPublishAfterProcessedBank(t *testing.T) {
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
		submitReceipt(t, index, signature, solana.Hash{9}, &deadline)
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
	stage := &submittedTransactionOutcomeStage{Sink: index}
	publishSubmittedTransactionOutcomes(stage, block, []string{"", "InstructionError", ""})

	for _, signature := range []solana.Signature{success, failed, pending} {
		if receipt, _ := index.Lookup(signature); receipt.Status != txstatus.StatusSubmitted {
			t.Fatalf("receipt %s changed before bank publication: %v", signature, receipt.Status)
		}
	}
	processed := &sealevel.SlotCtx{Slot: block.Slot}
	setter := &receiptObservingSlotCtxSetter{index: index, success: success, pending: pending}
	publishAcceptedProcessedBank(setter, processed, stage)
	if setter.published != processed ||
		setter.successAtPublication != txstatus.StatusSubmitted ||
		setter.pendingAtPublication != txstatus.StatusSubmitted {
		t.Fatalf("receipts published before processed bank: %+v", setter)
	}

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

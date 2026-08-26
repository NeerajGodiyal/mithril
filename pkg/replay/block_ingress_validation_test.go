package replay

import (
	"errors"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/arena"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/rewards"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

func TestNewSlotCtxOwnsVoteTimestamps(t *testing.T) {
	key := solana.PublicKey{0xa1}
	parent := map[solana.PublicKey]sealevel.BlockTimestamp{key: {Slot: 10, Timestamp: 20}}
	block := &b.Block{Slot: 11, VoteTimestamps: parent}
	slotCtx := newSlotCtx(block, accounts.NewMemAccounts(), accounts.NewMemAccounts(), nil, nil, 0)

	slotCtx.VoteTimestamps[key] = sealevel.BlockTimestamp{Slot: 11, Timestamp: 30}
	require.Equal(t, sealevel.BlockTimestamp{Slot: 10, Timestamp: 20}, parent[key])
}

func TestVerifyBlockTransactionSignaturesFailsClosed(t *testing.T) {
	valid, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	block := &b.Block{Slot: 77, Transactions: []*solana.Transaction{valid}}
	require.NoError(t, verifyBlockTransactionSignatures(block))
	require.True(t, block.TransactionSignaturesVerified())

	invalid := *valid
	invalid.Signatures = append([]solana.Signature(nil), valid.Signatures...)
	invalid.Signatures[0][0] ^= 0xff
	block = &b.Block{Slot: 78, Transactions: []*solana.Transaction{&invalid}}
	require.ErrorContains(t, verifyBlockTransactionSignatures(block), "slot 78 transaction 0")
	require.False(t, block.TransactionSignaturesVerified())

	wrongArity := *valid
	wrongArity.Signatures = nil
	block = &b.Block{Slot: 79, Transactions: []*solana.Transaction{&wrongArity}}
	require.ErrorContains(t, verifyBlockTransactionSignatures(block), "got 1 signers, but 0 signatures")
	require.False(t, block.TransactionSignaturesVerified())
}

func TestVerifyBlockTransactionSignaturesTrustsInMemoryMarker(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	tx.Signatures[0][0] ^= 0xff
	block := &b.Block{Slot: 80, Transactions: []*solana.Transaction{tx}}
	block.MarkTransactionSignaturesVerified()
	require.NoError(t, verifyBlockTransactionSignatures(block))
}

func TestValidateBlockTransactionMetadata(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		block   *b.Block
		wantErr string
	}{
		{"rpc missing", &b.Block{Slot: 1, Transactions: []*solana.Transaction{tx}}, "missing metadata"},
		{"rpc short", &b.Block{Slot: 1, Transactions: []*solana.Transaction{tx}, TxMetas: []*rpc.TransactionMeta{}}, "got 0 entries"},
		{"rpc nil entry", &b.Block{Slot: 1, Transactions: []*solana.Transaction{tx}, TxMetas: []*rpc.TransactionMeta{nil}}, "nil entry"},
		{"rpc complete", &b.Block{Slot: 1, Transactions: []*solana.Transaction{tx}, TxMetas: []*rpc.TransactionMeta{{}}}, ""},
		{"live absent", &b.Block{Slot: 1, FromLiveStream: true, Transactions: []*solana.Transaction{tx}}, ""},
		{"local absent", &b.Block{Slot: 1, FromLocalProduction: true, Transactions: []*solana.Transaction{tx}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBlockTransactionMetadata(tc.block)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestValidateBlockTransactionBalanceMetadata(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	accountCount := len(tx.Message.AccountKeys)

	for _, tc := range []struct {
		name string
		pre  int
		post int
		want bool
	}{
		{"exact", accountCount, accountCount, false},
		{"short pre", accountCount - 1, accountCount, true},
		{"short post", accountCount, accountCount - 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := &b.Block{
				Slot: 1, Transactions: []*solana.Transaction{tx},
				TxMetas: []*rpc.TransactionMeta{{PreBalances: make([]uint64, tc.pre), PostBalances: make([]uint64, tc.post)}},
			}
			err := validateBlockTransactionBalanceMetadata(block)
			if tc.want {
				require.ErrorContains(t, err, "transaction 0")
			} else {
				require.NoError(t, err)
			}
		})
	}

	expanded := *tx
	expanded.Message = tx.Message
	expanded.Message.AccountKeys = append(append([]solana.PublicKey(nil), tx.Message.AccountKeys...), solana.PublicKey{0xaa})
	block := &b.Block{
		Slot: 2, Transactions: []*solana.Transaction{&expanded},
		TxMetas: []*rpc.TransactionMeta{{PreBalances: make([]uint64, accountCount), PostBalances: make([]uint64, accountCount)}},
	}
	require.ErrorContains(t, validateBlockTransactionBalanceMetadata(block), "accounts=4")

	versioned := validShapeTransaction()
	versioned.Message.SetVersion(solana.MessageVersionV0)
	table := solana.PublicKey{0xbb}
	loaded := solana.PublicKey{0xcc}
	versioned.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{AccountKey: table, WritableIndexes: []uint8{0}}})
	require.NoError(t, versioned.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{table: {loaded}}))
	require.NoError(t, versioned.Message.ResolveLookups())
	block = &b.Block{
		Slot: 3, Transactions: []*solana.Transaction{versioned},
		TxMetas: []*rpc.TransactionMeta{{
			PreBalances: make([]uint64, len(versioned.Message.AccountKeys)), PostBalances: make([]uint64, len(versioned.Message.AccountKeys)),
			LoadedAddresses: rpc.LoadedAddresses{Writable: []solana.PublicKey{{0xdd}}},
		}},
	}
	require.ErrorContains(t, validateBlockTransactionBalanceMetadata(block), "does not match the parent bank")
	block.TxMetas[0].LoadedAddresses.Writable = []solana.PublicKey{loaded}
	require.NoError(t, validateBlockTransactionBalanceMetadata(block))

	legacy := validShapeTransaction()
	block = &b.Block{
		Slot: 4, Transactions: []*solana.Transaction{legacy},
		TxMetas: []*rpc.TransactionMeta{{
			PreBalances: make([]uint64, len(legacy.Message.AccountKeys)), PostBalances: make([]uint64, len(legacy.Message.AccountKeys)),
			LoadedAddresses: rpc.LoadedAddresses{ReadOnly: []solana.PublicKey{{0xee}}},
		}},
	}
	require.ErrorContains(t, validateBlockTransactionBalanceMetadata(block), "without lookups")
	block.TxMetas[0].LoadedAddresses.ReadOnly = nil
	require.NoError(t, validateBlockTransactionBalanceMetadata(block))
}

func TestPlanBlockTransactionExecutionRejectsMalformedHeaderBeforePlanner(t *testing.T) {
	tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
	require.NoError(t, err)
	tx.Message.Header.NumReadonlySignedAccounts = 2
	tx.Signatures = nil
	privateKey := txfixture.PayerPrivateKey()
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == txfixture.PayerPubkey() {
			return &privateKey
		}
		return nil
	})
	require.NoError(t, err)
	block := &b.Block{Slot: 81, FromLiveStream: true, Transactions: []*solana.Transaction{tx}}
	require.NoError(t, verifyBlockTransactionSignatures(block))

	_, err = planBlockTransactionExecution(block)
	require.ErrorIs(t, err, TxErrSanitizeFailure)
	require.NotPanics(t, func() {
		_, err = ProcessBlock(nil, block, nil, 1, nil, nil, nil, NewTransactionStatusCache(), false, nil)
	})
	require.ErrorIs(t, err, TxErrSanitizeFailure)
}

func TestTransactionLoopsRejectUnprocessableTransactionWithoutFees(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(map[bool]string{false: "sequential", true: "parallel"}[parallel], func(t *testing.T) {
			slotCtx, cleanup := newCommitTestSlotCtx()
			defer cleanup()
			tx, err := solana.TransactionFromBytes(txfixture.MustSignedTransferWire(0))
			require.NoError(t, err)
			payer, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)
			payer.Lamports = 1
			require.NoError(t, slotCtx.SetAccount(txfixture.PayerPubkey(), payer))

			block := &b.Block{Slot: slotCtx.Slot, Transactions: []*solana.Transaction{tx}}
			plan, err := planBlockTransactionExecution(block)
			require.NoError(t, err)
			var sigverify sync.WaitGroup
			if parallel {
				previousArenas := sealevel.BorrowedAccountArenas
				sealevel.BorrowedAccountArenas = []*arena.Arena[sealevel.BorrowedAccount]{arena.New[sealevel.BorrowedAccount](16)}
				defer func() { sealevel.BorrowedAccountArenas = previousArenas }()
				_, _, err = parallelTxLoop(slotCtx, &sigverify, block, block, plan, nil, 1, nil, false)
			} else {
				_, _, err = sequentialTxLoop(slotCtx, &sigverify, block, plan, nil, nil, false)
			}
			sigverify.Wait()
			require.ErrorContains(t, err, "not processable")
			var invalidCandidate *objectivelyInvalidAlpenglowCandidateError
			require.ErrorAs(t, err, &invalidCandidate)
			require.Equal(t, slotCtx.Slot, invalidCandidate.Slot)
			require.Zero(t, invalidCandidate.TransactionIndex)
			after, err := slotCtx.GetAccount(txfixture.PayerPubkey())
			require.NoError(t, err)
			require.Equal(t, uint64(1), after.Lamports)
		})
	}
}

func TestInvalidAlpenglowCandidateClassificationAndRecoveryBoundary(t *testing.T) {
	protocolErr := errors.New("insufficient fee payer")
	classified := classifyUnprocessableTransaction(91, 3, protocolErr)
	var invalidCandidate *objectivelyInvalidAlpenglowCandidateError
	require.ErrorAs(t, classified, &invalidCandidate)
	require.ErrorIs(t, classified, protocolErr)

	block := &b.Block{Slot: 91}
	quarantines := 0
	quarantine := func(got *b.Block) error {
		quarantines++
		require.Same(t, block, got)
		return nil
	}

	continueReplay, resultErr := handleObjectivelyInvalidAlpenglowCandidate(block, classified, false, quarantine)
	require.True(t, continueReplay)
	require.NoError(t, resultErr)
	require.Equal(t, 1, quarantines)

	continueReplay, resultErr = handleObjectivelyInvalidAlpenglowCandidate(block, classified, true, quarantine)
	require.False(t, continueReplay)
	require.Error(t, resultErr)
	require.True(t, RequiresExternalAlpenglowRepair(resultErr))
	require.ErrorIs(t, resultErr, protocolErr)
	require.Equal(t, 2, quarantines)
}

func TestInvalidAlpenglowCandidateNeverBlamesLocalAccountSource(t *testing.T) {
	storageErr := errors.New("captured account bank unavailable")
	sourceErr := newAccountSourceError("load payer", storageErr)
	classified := classifyUnprocessableTransaction(92, 0, sourceErr)
	require.Same(t, sourceErr, classified)
	var invalidCandidate *objectivelyInvalidAlpenglowCandidateError
	require.NotErrorAs(t, classified, &invalidCandidate)

	quarantines := 0
	continueReplay, resultErr := handleObjectivelyInvalidAlpenglowCandidate(
		&b.Block{Slot: 92}, classified, false,
		func(*b.Block) error { quarantines++; return nil },
	)
	require.False(t, continueReplay)
	require.Same(t, classified, resultErr)
	require.Zero(t, quarantines)
	require.ErrorIs(t, resultErr, storageErr)
}

func TestInvalidAlpenglowCandidateQuarantineFailureStopsReplay(t *testing.T) {
	protocolErr := errors.New("invalid recent blockhash")
	classified := classifyUnprocessableTransaction(93, 1, protocolErr)
	quarantineErr := errors.New("consensus sink rejected tombstone")

	continueReplay, resultErr := handleObjectivelyInvalidAlpenglowCandidate(
		&b.Block{Slot: 93}, classified, false,
		func(*b.Block) error { return quarantineErr },
	)
	require.False(t, continueReplay)
	require.ErrorContains(t, resultErr, quarantineErr.Error())
	require.ErrorIs(t, resultErr, protocolErr)
	require.False(t, RequiresExternalAlpenglowRepair(resultErr))
}

func TestInvalidAlpenglowCandidateExternalRepairPredicates(t *testing.T) {
	require.False(t, candidateRequiresExternalAlpenglowRepair(false, nil, false))
	require.True(t, candidateRequiresExternalAlpenglowRepair(true, nil, false), "epoch transition")
	require.True(t, candidateRequiresExternalAlpenglowRepair(false, &rewards.PartitionedRewardDistributionInfo{}, false), "reward transition")
	require.True(t, candidateRequiresExternalAlpenglowRepair(false, nil, true), "startup feature transition")
}

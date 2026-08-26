package rootedevents

import (
	"errors"
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/txstatus"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testTransactionObservation(t testing.TB) TransactionObservation {
	t.Helper()
	tx := &solana.Transaction{
		Signatures: []solana.Signature{{1}},
		Message: solana.Message{
			Header:          solana.MessageHeader{NumRequiredSignatures: 1},
			AccountKeys:     []solana.PublicKey{{1}},
			RecentBlockhash: solana.Hash{2},
		},
	}
	return testTransactionObservationFromTransaction(t, tx)
}

func testTransactionObservationFromTransaction(t testing.TB, tx *solana.Transaction) TransactionObservation {
	t.Helper()
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)
	hash, err := txstatus.TransactionMessageHash(tx)
	require.NoError(t, err)
	return TransactionObservation{
		Signature: tx.Signatures[0].String(), Transaction: wire,
		MessageHash: solana.Hash(hash).String(), AccountKeys: []string{solana.PublicKey{1}.String()}, Succeeded: true,
	}
}

func testSlotMeta(slot, parent uint64) SlotMeta {
	return SlotMeta{
		Slot: slot, ParentSlot: parent, Blockhash: [32]byte{byte(slot + 1)},
		ParentBlockhash: [32]byte{byte(parent + 1)}, Bankhash: [32]byte{byte(slot + 2)},
		FinalitySource: FinalityRPCFinalized,
	}
}

func TestBuildEventsDeterministicAndOwned(t *testing.T) {
	keyA := solana.PublicKey{1}
	keyB := solana.PublicKey{2}
	owner := solana.PublicKey{9}
	data := []byte{4, 5, 6}
	deltas := []accounts.SlotDelta{
		{Slot: 10, Delta: []*accounts.Account{
			{Key: keyB, Owner: [32]byte(owner), Lamports: 0},
			{Key: keyA, Owner: [32]byte(owner), Lamports: 7, Data: data, Executable: true, RentEpoch: 8},
		}},
		{Slot: 12},
	}
	meta10 := testSlotMeta(10, 9)
	transaction := testTransactionObservation(t)
	transaction.AccountKeys = []string{keyA.String()}
	transaction.ComputeUnits = 12
	transaction.Logs = []string{"Program log: rooted"}
	meta10.Transactions = []TransactionObservation{transaction}
	meta12 := testSlotMeta(12, 10)
	metadata := map[uint64]SlotMeta{10: meta10, 12: meta12}

	events, err := BuildEvents(deltas, metadata)
	require.NoError(t, err)
	require.Len(t, events, 5)

	require.Equal(t, Cursor{Slot: 10, Ordinal: 0}, events[0].Cursor)
	require.Equal(t, TransactionExecuted, events[0].Kind)
	require.Equal(t, solana.Signature{1}.String(), events[0].Transaction.Signature)
	require.Equal(t, []string{"Program log: rooted"}, events[0].Transaction.Logs)
	require.Equal(t, Cursor{Slot: 10, Ordinal: 1}, events[1].Cursor)
	require.Equal(t, keyA.String(), events[1].Account.Pubkey)
	require.Equal(t, []byte{4, 5, 6}, events[1].Account.Data)
	require.False(t, events[1].Account.Tombstone)
	require.Equal(t, Cursor{Slot: 10, Ordinal: 2}, events[2].Cursor)
	require.Equal(t, keyB.String(), events[2].Account.Pubkey)
	require.True(t, events[2].Account.Tombstone)
	require.Equal(t, Cursor{Slot: 10, Ordinal: 3}, events[3].Cursor)
	require.Equal(t, &RootedSlot{
		ParentSlot: 9, Blockhash: solana.Hash(meta10.Blockhash).String(), ParentBlockhash: solana.Hash(meta10.ParentBlockhash).String(),
		Bankhash: solana.Hash(meta10.Bankhash).String(), FinalitySource: FinalityRPCFinalized,
		TransactionCount: 1, AccountCount: 2,
	}, events[3].Root)
	require.Equal(t, Cursor{Slot: 12, Ordinal: 0}, events[4].Cursor)
	require.Equal(t, uint32(0), events[4].Root.AccountCount)

	data[0] = 99
	metadata[10].Transactions[0].Logs[0] = "changed"
	require.Equal(t, byte(4), events[1].Account.Data[0], "event must own account data")
	require.Equal(t, "Program log: rooted", events[0].Transaction.Logs[0], "event must own transaction data")
	require.Equal(t, keyB, deltas[0].Delta[0].Key, "builder must not reorder the input")

	after, err := EventsAfter(events, &events[1].Cursor)
	require.NoError(t, err)
	require.Equal(t, events[2:], after)
}

func TestCloneTransactionObservationsOwnsNestedData(t *testing.T) {
	input := []TransactionObservation{{
		Transaction: []byte{1},
		AccountKeys: []string{"key"},
		Logs:        []string{"log"},
		Inner: []InnerInstructions{{Instructions: []CompiledInstruction{{
			Accounts: []uint16{2}, Data: []byte{3},
		}}}},
		ReturnData: &ReturnData{ProgramID: "program", Data: []byte{4}},
	}}
	got := CloneTransactionObservations(input)
	input[0].Transaction[0] = 9
	input[0].AccountKeys[0] = "changed"
	input[0].Logs[0] = "changed"
	input[0].Inner[0].Instructions[0].Accounts[0] = 9
	input[0].Inner[0].Instructions[0].Data[0] = 9
	input[0].ReturnData.Data[0] = 9

	if got[0].Transaction[0] != 1 || got[0].AccountKeys[0] != "key" || got[0].Logs[0] != "log" ||
		got[0].Inner[0].Instructions[0].Accounts[0] != 2 || got[0].Inner[0].Instructions[0].Data[0] != 3 ||
		got[0].ReturnData.Data[0] != 4 {
		t.Fatalf("clone shares nested storage: %+v", got[0])
	}
}

func TestBuildEventsRejectsMalformedTransaction(t *testing.T) {
	meta := testSlotMeta(10, 9)
	meta.Transactions = []TransactionObservation{{Index: 1, Signature: "invalid", Transaction: []byte{1}, Succeeded: true}}
	metadata := map[uint64]SlotMeta{10: meta}
	_, err := BuildEvents([]accounts.SlotDelta{{Slot: 10}}, metadata)
	require.Error(t, err)
}

func TestValidateTransactionEnforcesWireBounds(t *testing.T) {
	valid := func() TransactionObservation {
		return testTransactionObservation(t)
	}

	tests := []struct {
		name   string
		change func(*TransactionObservation)
		want   string
	}{
		{
			name: "oversized transaction",
			change: func(tx *TransactionObservation) {
				tx.Transaction = make([]byte, solana.MaxTransactionSizeV1+1)
			},
			want: "wire size",
		},
		{
			name: "missing account keys",
			change: func(tx *TransactionObservation) {
				tx.AccountKeys = nil
			},
			want: "account count",
		},
		{
			name: "oversized logs",
			change: func(tx *TransactionObservation) {
				tx.Logs = []string{string(make([]byte, maxTransactionLogBytes+1))}
			},
			want: "logs exceed",
		},
		{
			name: "too many inner instructions",
			change: func(tx *TransactionObservation) {
				tx.Inner = []InnerInstructions{{Instructions: make([]CompiledInstruction, maxInnerInstructions+1)}}
			},
			want: "runtime bounds",
		},
		{
			name: "invalid inner account index",
			change: func(tx *TransactionObservation) {
				tx.Inner = []InnerInstructions{{Instructions: []CompiledInstruction{{Accounts: []uint16{1}}}}}
			},
			want: "account index",
		},
		{
			name: "oversized return data",
			change: func(tx *TransactionObservation) {
				tx.ReturnData = &ReturnData{ProgramID: solana.PublicKey{1}.String(), Data: make([]byte, maxReturnDataBytes+1)}
			},
			want: "return data exceeds",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := valid()
			tt.change(&tx)
			require.ErrorContains(t, validateTransaction(10, 0, tx), tt.want)
		})
	}
}

func TestValidateTransactionUsesVersionSpecificWireLimit(t *testing.T) {
	makeTransaction := func(version solana.MessageVersion) *solana.Transaction {
		message := solana.Message{
			Header:          solana.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 1},
			AccountKeys:     []solana.PublicKey{{1}, {2}},
			RecentBlockhash: solana.Hash{3},
			Instructions:    []solana.CompiledInstruction{{ProgramIDIndex: 1, Data: make([]byte, 1300)}},
		}
		if version != solana.MessageVersionLegacy {
			_, err := message.SetVersion(version)
			require.NoError(t, err)
		}
		return &solana.Transaction{Signatures: []solana.Signature{{1}}, Message: message}
	}

	legacy := testTransactionObservationFromTransaction(t, makeTransaction(solana.MessageVersionLegacy))
	require.Greater(t, len(legacy.Transaction), maxLegacyTransactionBytes)
	require.ErrorContains(t, validateTransaction(10, 0, legacy), "legacy/v0 wire exceeds")

	v1 := testTransactionObservationFromTransaction(t, makeTransaction(solana.MessageVersionV1))
	require.LessOrEqual(t, len(v1.Transaction), solana.MaxTransactionSizeV1)
	require.NoError(t, validateTransaction(10, 0, v1))
}

func TestBuildEventsRejectsOversizedAccountData(t *testing.T) {
	key := solana.PublicKey{1}
	_, err := BuildEvents(
		[]accounts.SlotDelta{{Slot: 10, Delta: []*accounts.Account{{Key: key, Data: make([]byte, maxAccountDataBytes+1)}}}},
		map[uint64]SlotMeta{10: testSlotMeta(10, 9)},
	)
	require.ErrorContains(t, err, "data exceeds")
}

func TestValidateEventCountRejectsCombinedOrdinalOverflow(t *testing.T) {
	require.NoError(t, validateEventCount(10, 1, math.MaxUint32-1))
	require.ErrorContains(t, validateEventCount(10, 1, math.MaxUint32), "too many transaction and account events")
}

func TestBuildEventsRejectsIncompleteLineage(t *testing.T) {
	account := &accounts.Account{Key: solana.PublicKey{1}}
	tests := []struct {
		name     string
		deltas   []accounts.SlotDelta
		metadata map[uint64]SlotMeta
	}{
		{
			name:     "missing metadata",
			deltas:   []accounts.SlotDelta{{Slot: 10}},
			metadata: map[uint64]SlotMeta{},
		},
		{
			name:   "wrong parent",
			deltas: []accounts.SlotDelta{{Slot: 10}, {Slot: 12}},
			metadata: map[uint64]SlotMeta{
				10: testSlotMeta(10, 9),
				12: testSlotMeta(12, 9),
			},
		},
		{
			name:   "duplicate account",
			deltas: []accounts.SlotDelta{{Slot: 10, Delta: []*accounts.Account{account, account}}},
			metadata: map[uint64]SlotMeta{
				10: testSlotMeta(10, 9),
			},
		},
		{
			name:   "empty bankhash",
			deltas: []accounts.SlotDelta{{Slot: 10}},
			metadata: map[uint64]SlotMeta{10: {
				Slot: 10, ParentSlot: 9, Blockhash: [32]byte{11}, ParentBlockhash: [32]byte{10},
				FinalitySource: FinalityRPCFinalized,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildEvents(tt.deltas, tt.metadata)
			require.Error(t, err)
		})
	}
}

func TestBuildEventsBindsAlpenglowBlockLineage(t *testing.T) {
	first := testSlotMeta(10, 9)
	first.FinalitySource = FinalityAlpenglowCertificate
	first.HasAlpenglowBlockID = true
	first.HasAlpenglowParentBlockID = true
	first.AlpenglowBlockID = [32]byte{10}
	first.AlpenglowParentBlockID = [32]byte{9}
	second := testSlotMeta(12, 10)
	second.FinalitySource = FinalityAlpenglowDelegated
	second.HasAlpenglowBlockID = true
	second.HasAlpenglowParentBlockID = true
	second.AlpenglowBlockID = [32]byte{12}
	second.AlpenglowParentBlockID = first.AlpenglowBlockID

	events, err := BuildEvents(
		[]accounts.SlotDelta{{Slot: 10}, {Slot: 12}},
		map[uint64]SlotMeta{10: first, 12: second},
	)
	require.NoError(t, err)
	require.Equal(t, solana.Hash(first.AlpenglowBlockID).String(), events[0].Root.BlockID)
	require.Equal(t, solana.Hash(first.AlpenglowParentBlockID).String(), events[0].Root.ParentBlockID)
	require.Equal(t, FinalityAlpenglowCertificate, events[0].Root.FinalitySource)
	require.Equal(t, FinalityAlpenglowDelegated, events[len(events)-1].Root.FinalitySource)

	second.AlpenglowParentBlockID = [32]byte{99}
	_, err = BuildEvents(
		[]accounts.SlotDelta{{Slot: 10}, {Slot: 12}},
		map[uint64]SlotMeta{10: first, 12: second},
	)
	require.ErrorContains(t, err, "parent block ID")
}

func TestEventsAfterRejectsUnknownCursor(t *testing.T) {
	_, err := EventsAfter([]Event{{Cursor: Cursor{Slot: 3, Ordinal: 0}}}, &Cursor{Slot: 2, Ordinal: 0})
	require.True(t, errors.Is(err, ErrCursorNotFound))
}

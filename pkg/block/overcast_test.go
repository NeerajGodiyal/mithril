package block

import (
	"math"
	"strings"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/overcast"
	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestFromLightbringerStreamMsgLeavesBlockHeightUnset(t *testing.T) {
	var entryHash [32]byte
	entryHash[0] = 0xAB

	resp := &overcast.SlotResponse{
		Slot:       123,
		ParentSlot: 122,
		Entries: []*overcast.Entry{
			{
				NumHashes: 1,
				Hash:      entryHash[:],
			},
		},
	}

	block, err := FromLightbringerStreamMsg(resp)
	require.NoError(t, err)

	if block.BlockHeight != 0 {
		t.Fatalf("expected block height to be unset, got %d", block.BlockHeight)
	}
	if block.SourceParentSlot != 122 {
		t.Fatalf("expected parent slot 122, got %d", block.SourceParentSlot)
	}
	if block.Blockhash != solana.HashFromBytes(entryHash[:]) {
		t.Fatalf("expected blockhash %s, got %s", solana.HashFromBytes(entryHash[:]), block.Blockhash)
	}
}

func TestFromLightbringerStreamMsgRejectsUnknownTransactionVariant(t *testing.T) {
	entryHash := make([]byte, overcastHashSize)
	entryHash[0] = 0xAB

	// Field 5 is outside the supported message oneof. Future variants must
	// remain an explicit decode failure rather than silently losing fields.
	futureTx := &overcast.VersionedTransaction{}
	futureTx.ProtoReflect().SetUnknown([]byte{0x2a, 0x00})
	resp := &overcast.SlotResponse{
		Slot: 123,
		Entries: []*overcast.Entry{{
			Hash:         entryHash,
			Transactions: []*overcast.VersionedTransaction{futureTx},
		}},
	}

	var (
		got *Block
		err error
	)
	require.NotPanics(t, func() {
		got, err = FromLightbringerStreamMsg(resp)
	})
	require.Nil(t, got)
	require.ErrorContains(t, err, "unsupported transaction fields")
}

func TestFromLightbringerStreamMsgRejectsMissingTransactionMessage(t *testing.T) {
	resp := &overcast.SlotResponse{
		Slot: 123,
		Entries: []*overcast.Entry{{
			Hash:         make([]byte, overcastHashSize),
			Transactions: []*overcast.VersionedTransaction{{}},
		}},
	}

	got, err := FromLightbringerStreamMsg(resp)
	require.Nil(t, got)
	require.ErrorContains(t, err, "unsupported or missing transaction message")
}

func TestFromLightbringerStreamMsgConvertsV0(t *testing.T) {
	entryHash := make([]byte, overcastHashSize)
	recentBlockhash := make([]byte, overcastHashSize)
	accountKey := make([]byte, overcastPubkeySize)
	tableKey := make([]byte, overcastPubkeySize)
	resp := &overcast.SlotResponse{
		Slot: 123,
		Entries: []*overcast.Entry{{
			Hash: entryHash,
			Transactions: []*overcast.VersionedTransaction{{
				Signatures: [][]byte{make([]byte, overcastSignatureSize)},
				Message: &overcast.VersionedTransaction_MessageV0{
					MessageV0: &overcast.VersionedMessageV0{
						Header: &overcast.MessageHeader{
							NumRequiredSignatures: 1,
						},
						AccountKeys:     [][]byte{accountKey},
						RecentBlockhash: recentBlockhash,
						AddressTableLookups: []*overcast.MessageAddressTableLookup{{
							AccountKey:      tableKey,
							WritableIndexes: []byte{1},
						}},
					},
				},
			}},
		}},
	}

	got, err := FromLightbringerStreamMsg(resp)
	require.NoError(t, err)
	require.Len(t, got.Transactions, 1)
	require.Equal(t, solana.MessageVersionV0, got.Transactions[0].Message.GetVersion())
	require.Equal(t, []uint8{uint8(solana.MessageVersionV0)}, got.Versions)
}

func TestFromLightbringerStreamMsgSanitizesConvertedTransaction(t *testing.T) {
	resp := &overcast.SlotResponse{
		Slot: 123,
		Entries: []*overcast.Entry{{
			Hash: make([]byte, overcastHashSize),
			Transactions: []*overcast.VersionedTransaction{{
				Signatures: [][]byte{
					make([]byte, overcastSignatureSize),
					make([]byte, overcastSignatureSize),
				},
				Message: &overcast.VersionedTransaction_MessageLegacy{
					MessageLegacy: &overcast.VersionedMessageLegacy{
						Header: &overcast.MessageHeader{
							NumRequiredSignatures: 2,
						},
						AccountKeys:     [][]byte{make([]byte, overcastPubkeySize)},
						RecentBlockhash: make([]byte, overcastHashSize),
					},
				},
			}},
		}},
	}

	var (
		got *Block
		err error
	)
	require.NotPanics(t, func() {
		got, err = FromLightbringerStreamMsg(resp)
	})
	require.Nil(t, got)
	require.ErrorContains(t, err, "more signatures (2) than static account keys (1)")
}
func TestFromLightbringerStreamMsgCarriesV1ProducerPayload(t *testing.T) {
	priorityFee := uint64(50_000)
	computeUnitLimit := uint32(200_000)
	loadedAccountsLimit := uint32(65_536)
	heapSize := uint32(64 * 1024)
	accountKey := make([]byte, len(solana.PublicKey{}))
	accountKey[0] = 0x44
	programKey := make([]byte, len(solana.PublicKey{}))
	programKey[0] = 0x45
	lifetimeSpecifier := make([]byte, len(solana.Hash{}))
	lifetimeSpecifier[0] = 0x55

	producerTransaction := &overcast.VersionedTransaction{
		Signatures: [][]byte{make([]byte, len(solana.Signature{}))},
		Message: &overcast.VersionedTransaction_MessageV1{MessageV1: &overcast.VersionedMessageV1{
			Header: &overcast.MessageHeader{NumRequiredSignatures: 1},
			Config: &overcast.TransactionConfig{
				PriorityFee:                 &priorityFee,
				ComputeUnitLimit:            &computeUnitLimit,
				LoadedAccountsDataSizeLimit: &loadedAccountsLimit,
				HeapSize:                    &heapSize,
			},
			LifetimeSpecifier: lifetimeSpecifier,
			AccountKeys:       [][]byte{accountKey, programKey},
			Instructions: []*overcast.CompiledInstruction{{
				ProgramIdIndex: 1,
				Data:           []byte{0x66},
			}},
		}},
	}
	wire, err := proto.Marshal(producerTransaction)
	if err != nil {
		t.Fatalf("marshal Lightbringer v1 transaction: %v", err)
	}
	decoded := new(overcast.VersionedTransaction)
	if err := proto.Unmarshal(wire, decoded); err != nil {
		t.Fatalf("unmarshal Lightbringer v1 transaction: %v", err)
	}

	entryHash := make([]byte, len(solana.Hash{}))
	entryHash[0] = 0x77
	block, err := FromLightbringerStreamMsg(&overcast.SlotResponse{
		Slot:       123,
		ParentSlot: 122,
		Entries: []*overcast.Entry{{
			NumHashes:    1,
			Hash:         entryHash,
			Transactions: []*overcast.VersionedTransaction{decoded},
		}},
	})
	if err != nil {
		t.Fatalf("convert Lightbringer v1 block: %v", err)
	}
	if got := block.Versions; len(got) != 1 || got[0] != uint8(solana.MessageVersionV1) {
		t.Fatalf("versions = %v, want [%d]", got, solana.MessageVersionV1)
	}
	tx := block.Transactions[0]
	if got := tx.Message.GetVersion(); got != solana.MessageVersionV1 {
		t.Fatalf("message version = %d, want %d", got, solana.MessageVersionV1)
	}
	if tx.Message.RecentBlockhash != solana.HashFromBytes(lifetimeSpecifier) {
		t.Fatalf("lifetime specifier = %s, want %s", tx.Message.RecentBlockhash, solana.HashFromBytes(lifetimeSpecifier))
	}
	config := tx.Message.TransactionConfig
	if config.PriorityFee == nil || *config.PriorityFee != priorityFee ||
		config.ComputeUnitLimit == nil || *config.ComputeUnitLimit != computeUnitLimit ||
		config.LoadedAccountsDataSizeLimit == nil || *config.LoadedAccountsDataSizeLimit != loadedAccountsLimit ||
		config.HeapSize == nil || *config.HeapSize != heapSize {
		t.Fatalf("transaction config = %+v, want all producer values preserved", config)
	}

	transactionWire, err := tx.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal converted v1 transaction: %v", err)
	}
	roundTrip, err := solana.TransactionFromBytes(transactionWire)
	if err != nil {
		t.Fatalf("decode converted v1 transaction: %v", err)
	}
	if roundTrip.Message.GetVersion() != solana.MessageVersionV1 ||
		roundTrip.Message.TransactionConfig.PriorityFee == nil ||
		*roundTrip.Message.TransactionConfig.PriorityFee != priorityFee {
		t.Fatalf("v1 wire round trip lost version or priority fee: %+v", roundTrip.Message.TransactionConfig)
	}
}

func TestFromLightbringerStreamMsgKeepsLegacyAndV0Compatible(t *testing.T) {
	recentBlockhash := make([]byte, len(solana.Hash{}))
	header := &overcast.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 1}
	payerKey := make([]byte, len(solana.PublicKey{}))
	payerKey[0] = 0x11
	programKey := make([]byte, len(solana.PublicKey{}))
	programKey[0] = 0x22
	accountKeys := [][]byte{payerKey, programKey}
	signatures := [][]byte{make([]byte, len(solana.Signature{}))}
	lookupKey := make([]byte, len(solana.PublicKey{}))
	lookupKey[0] = 0x88
	tests := []struct {
		name        string
		transaction *overcast.VersionedTransaction
		wantVersion solana.MessageVersion
		wantLookups int
	}{
		{
			name: "legacy",
			transaction: &overcast.VersionedTransaction{Signatures: signatures, Message: &overcast.VersionedTransaction_MessageLegacy{
				MessageLegacy: &overcast.VersionedMessageLegacy{Header: header, AccountKeys: accountKeys, RecentBlockhash: recentBlockhash},
			}},
			wantVersion: solana.MessageVersionLegacy,
		},
		{
			name: "v0",
			transaction: &overcast.VersionedTransaction{Signatures: signatures, Message: &overcast.VersionedTransaction_MessageV0{
				MessageV0: &overcast.VersionedMessageV0{
					Header: header, AccountKeys: accountKeys, RecentBlockhash: recentBlockhash,
					AddressTableLookups: []*overcast.MessageAddressTableLookup{{
						AccountKey: lookupKey, WritableIndexes: []byte{1}, ReadonlyIndexes: []byte{2},
					}},
				},
			}},
			wantVersion: solana.MessageVersionV0,
			wantLookups: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			producerWire, err := proto.Marshal(test.transaction)
			if err != nil {
				t.Fatalf("marshal producer transaction: %v", err)
			}
			decoded := new(overcast.VersionedTransaction)
			if err := proto.Unmarshal(producerWire, decoded); err != nil {
				t.Fatalf("unmarshal producer transaction: %v", err)
			}
			entryHash := make([]byte, len(solana.Hash{}))
			block, err := FromLightbringerStreamMsg(&overcast.SlotResponse{Entries: []*overcast.Entry{{
				Hash: entryHash, Transactions: []*overcast.VersionedTransaction{decoded},
			}}})
			if err != nil {
				t.Fatalf("convert Lightbringer block: %v", err)
			}
			if got := block.Transactions[0].Message.GetVersion(); got != test.wantVersion {
				t.Fatalf("message version = %d, want %d", got, test.wantVersion)
			}
			if got := len(block.Transactions[0].Message.AddressTableLookups); got != test.wantLookups {
				t.Fatalf("address table lookups = %d, want %d", got, test.wantLookups)
			}
			if !block.Transactions[0].Message.TransactionConfig.IsEmpty() {
				t.Fatal("legacy/v0 transaction gained a v1 config")
			}
		})
	}
}

func TestFromLightbringerStreamMsgRejectsProgramIDIndexOutsideWireRange(t *testing.T) {
	_, err := FromLightbringerStreamMsg(&overcast.SlotResponse{Entries: []*overcast.Entry{{
		Hash: make([]byte, len(solana.Hash{})),
		Transactions: []*overcast.VersionedTransaction{{
			Message: &overcast.VersionedTransaction_MessageV1{MessageV1: &overcast.VersionedMessageV1{
				Header:            &overcast.MessageHeader{},
				LifetimeSpecifier: make([]byte, len(solana.Hash{})),
				Instructions: []*overcast.CompiledInstruction{{
					ProgramIdIndex: math.MaxUint8 + 1,
				}},
			}},
		}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "program ID index 256 exceeds u8 limit") {
		t.Fatalf("program id index outside the Solana wire range error = %v", err)
	}
}

func TestFromLightbringerStreamMsgRejectsUnsanitizedTransaction(t *testing.T) {
	accountKeys := [][]byte{
		make([]byte, len(solana.PublicKey{})),
		make([]byte, len(solana.PublicKey{})),
	}
	accountKeys[1][0] = 1
	_, err := FromLightbringerStreamMsg(&overcast.SlotResponse{Entries: []*overcast.Entry{{
		Hash: make([]byte, len(solana.Hash{})),
		Transactions: []*overcast.VersionedTransaction{{
			Signatures: [][]byte{make([]byte, len(solana.Signature{}))},
			Message: &overcast.VersionedTransaction_MessageV1{MessageV1: &overcast.VersionedMessageV1{
				Header:            &overcast.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 1},
				LifetimeSpecifier: make([]byte, len(solana.Hash{})),
				AccountKeys:       accountKeys,
				Instructions: []*overcast.CompiledInstruction{{
					ProgramIdIndex: 0,
				}},
			}},
		}},
	}}})
	if err == nil || !strings.Contains(err.Error(), "invalid transaction version") {
		t.Fatalf("unsanitized transaction error = %v", err)
	}
}

func TestFromLightbringerStreamMsgRejectsOversizedTransaction(t *testing.T) {
	header := &overcast.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 1}
	accountKeys := [][]byte{
		make([]byte, len(solana.PublicKey{})),
		make([]byte, len(solana.PublicKey{})),
	}
	accountKeys[1][0] = 1
	signatures := [][]byte{make([]byte, len(solana.Signature{}))}
	blockhash := make([]byte, len(solana.Hash{}))
	tests := []struct {
		name        string
		transaction *overcast.VersionedTransaction
	}{
		{
			name: "legacy",
			transaction: &overcast.VersionedTransaction{Signatures: signatures, Message: &overcast.VersionedTransaction_MessageLegacy{
				MessageLegacy: &overcast.VersionedMessageLegacy{
					Header: header, AccountKeys: accountKeys, RecentBlockhash: blockhash,
					Instructions: []*overcast.CompiledInstruction{{ProgramIdIndex: 1, Data: make([]byte, txverify.MaxLegacyTransactionSize)}},
				},
			}},
		},
		{
			name: "v0",
			transaction: &overcast.VersionedTransaction{Signatures: signatures, Message: &overcast.VersionedTransaction_MessageV0{
				MessageV0: &overcast.VersionedMessageV0{
					Header: header, AccountKeys: accountKeys, RecentBlockhash: blockhash,
					Instructions: []*overcast.CompiledInstruction{{ProgramIdIndex: 1, Data: make([]byte, txverify.MaxLegacyTransactionSize)}},
				},
			}},
		},
		{
			name: "v1",
			transaction: &overcast.VersionedTransaction{Signatures: signatures, Message: &overcast.VersionedTransaction_MessageV1{
				MessageV1: &overcast.VersionedMessageV1{
					Header: header, AccountKeys: accountKeys, LifetimeSpecifier: blockhash,
					Instructions: []*overcast.CompiledInstruction{{ProgramIdIndex: 1, Data: make([]byte, solana.MaxTransactionSizeV1)}},
				},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := FromLightbringerStreamMsg(&overcast.SlotResponse{Entries: []*overcast.Entry{{
				Hash: make([]byte, len(solana.Hash{})), Transactions: []*overcast.VersionedTransaction{test.transaction},
			}}})
			if err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("oversized transaction error = %v", err)
			}
		})
	}
}

func TestFromLightbringerStreamMsgRejectsNilMessagePayloads(t *testing.T) {
	for name, tx := range map[string]*overcast.VersionedTransaction{
		"legacy wrapper": {Message: (*overcast.VersionedTransaction_MessageLegacy)(nil)},
		"legacy payload": {Message: &overcast.VersionedTransaction_MessageLegacy{}},
		"v0 wrapper":     {Message: (*overcast.VersionedTransaction_MessageV0)(nil)},
		"v0 payload":     {Message: &overcast.VersionedTransaction_MessageV0{}},
		"v1 wrapper":     {Message: (*overcast.VersionedTransaction_MessageV1)(nil)},
		"v1 payload":     {Message: &overcast.VersionedTransaction_MessageV1{}},
	} {
		t.Run(name, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, _, err := overcastTransactionToTransaction(tx)
				require.ErrorContains(t, err, "no message payload")
			})
		})
	}
}

func TestOvercastInstructionRejectsProgramIndexTruncation(t *testing.T) {
	for _, index := range []uint32{256, 65536, math.MaxUint32} {
		_, err := overcastInstrToInstr(&overcast.CompiledInstruction{ProgramIdIndex: index})
		require.ErrorContains(t, err, "exceeds u8 limit")
	}
}

func TestOvercastV1PreservesOptionalZeroConfiguration(t *testing.T) {
	zeroFee, zeroUnits := uint64(0), uint32(0)
	for name, config := range map[string]*overcast.TransactionConfig{
		"absent":        nil,
		"empty":         {},
		"explicit zero": {PriorityFee: &zeroFee, ComputeUnitLimit: &zeroUnits, LoadedAccountsDataSizeLimit: &zeroUnits},
	} {
		t.Run(name, func(t *testing.T) {
			tx, _, err := overcastTransactionToTransaction(&overcast.VersionedTransaction{
				Signatures: [][]byte{make([]byte, overcastSignatureSize)},
				Message: &overcast.VersionedTransaction_MessageV1{MessageV1: &overcast.VersionedMessageV1{
					Header:            &overcast.MessageHeader{NumRequiredSignatures: 1},
					Config:            config,
					AccountKeys:       [][]byte{make([]byte, overcastPubkeySize)},
					LifetimeSpecifier: make([]byte, overcastHashSize),
				}},
			})
			require.NoError(t, err)
			raw, err := tx.MarshalBinary()
			require.NoError(t, err)
			decoded, err := solana.TransactionFromBytes(raw)
			require.NoError(t, err)
			want := solana.TransactionConfig{}
			if config != nil {
				want.PriorityFee, want.ComputeUnitLimit = config.PriorityFee, config.ComputeUnitLimit
				want.LoadedAccountsDataSizeLimit = config.LoadedAccountsDataSizeLimit
			}
			require.Equal(t, want, decoded.Message.TransactionConfig)
		})
	}
}

package txstatus

import (
	"bytes"
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestTransactionMessageIdentityStableAcrossV0LookupResolution(t *testing.T) {
	tableID := solana.PublicKey{0x70}
	dynamicWritable := solana.PublicKey{0x71}
	dynamicReadonly := solana.PublicKey{0x72}
	tx := &solana.Transaction{
		Message: solana.Message{
			Header: solana.MessageHeader{
				NumRequiredSignatures:       1,
				NumReadonlyUnsignedAccounts: 1,
			},
			AccountKeys:     []solana.PublicKey{{0x11}, {0x22}},
			RecentBlockhash: solana.Hash{0x33},
			Instructions: []solana.CompiledInstruction{{
				ProgramIDIndex: 1,
				Accounts:       []uint16{0, 2, 3},
				Data:           []byte{0x44},
			}},
		},
	}
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey:      tableID,
		WritableIndexes: []byte{0},
		ReadonlyIndexes: []byte{1},
	}})

	beforeWire, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("serialize unresolved v0 message: %v", err)
	}
	before, err := IdentityForTransaction(tx)
	if err != nil {
		t.Fatalf("hash unresolved v0 message: %v", err)
	}

	if err := tx.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
		tableID: {dynamicWritable, dynamicReadonly},
	}); err != nil {
		t.Fatalf("set address tables: %v", err)
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		t.Fatalf("resolve address-table lookups: %v", err)
	}
	if len(tx.Message.AccountKeys) != 4 {
		t.Fatalf("resolved account-key count = %d, want 4", len(tx.Message.AccountKeys))
	}

	afterWire, err := tx.Message.MarshalBinary()
	if err != nil {
		t.Fatalf("serialize resolved v0 message: %v", err)
	}
	after, err := IdentityForTransaction(tx)
	if err != nil {
		t.Fatalf("hash resolved v0 message: %v", err)
	}
	if !bytes.Equal(beforeWire, afterWire) {
		t.Fatalf("canonical v0 message changed across lookup resolution:\nbefore %x\nafter  %x", beforeWire, afterWire)
	}
	if before != after {
		t.Fatalf("message identity changed across lookup resolution: before %+v, after %+v", before, after)
	}
}

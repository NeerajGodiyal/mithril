package block

import (
	"testing"

	"github.com/gagliardetto/solana-go"
)

func TestPreparedTransactionMessageIdentitySurvivesV0LookupResolution(t *testing.T) {
	tableID := solana.PublicKey{0x70}
	tx := identityTestTransaction(1)
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey:      tableID,
		WritableIndexes: []byte{0},
		ReadonlyIndexes: []byte{1},
	}})
	block := &Block{Transactions: []*solana.Transaction{tx}}
	before, err := block.PrepareTransactionMessageIdentities()
	if err != nil {
		t.Fatalf("prepare unresolved identity: %v", err)
	}

	if err := tx.Message.SetAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
		tableID: {{0x71}, {0x72}},
	}); err != nil {
		t.Fatalf("set address table: %v", err)
	}
	if err := tx.Message.ResolveLookups(); err != nil {
		t.Fatalf("resolve address-table lookups: %v", err)
	}
	after, err := block.PrepareTransactionMessageIdentities()
	if err != nil {
		t.Fatalf("reuse resolved identity: %v", err)
	}
	if before != after {
		t.Fatal("address-table resolution rebuilt an identity whose canonical message is unchanged")
	}
	if before.Identity(0) != after.Identity(0) {
		t.Fatalf("identity changed across lookup resolution: before %+v, after %+v", before.Identity(0), after.Identity(0))
	}
}

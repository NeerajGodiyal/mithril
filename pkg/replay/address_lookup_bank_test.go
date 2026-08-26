package replay

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

type lookupBankReader struct {
	key     solana.PublicKey
	account *accounts.Account
	reads   int
}

func (r *lookupBankReader) GetAccount(_ uint64, key solana.PublicKey) (*accounts.Account, error) {
	r.reads++
	if key != r.key {
		return nil, fmt.Errorf("unexpected account %s", key)
	}
	return r.account.Clone(), nil
}

func lookupTableAccount(key solana.PublicKey, deactivationSlot, lastExtendedSlot uint64, startIndex byte, addresses ...solana.PublicKey) *accounts.Account {
	data := make([]byte, sealevel.AddressLookupTableMetaSize+len(addresses)*solana.PublicKeyLength)
	binary.LittleEndian.PutUint32(data, sealevel.AddressLookupTableProgramStateLookupTable)
	binary.LittleEndian.PutUint64(data[4:], deactivationSlot)
	binary.LittleEndian.PutUint64(data[12:], lastExtendedSlot)
	data[20] = startIndex
	for i, address := range addresses {
		copy(data[sealevel.AddressLookupTableMetaSize+i*solana.PublicKeyLength:], address[:])
	}
	return &accounts.Account{
		Key:      key,
		Lamports: 1,
		Data:     data,
		Owner:    [32]byte(a.AddressLookupTableAddr),
	}
}

func lookupSlotCtx(t *testing.T, slot uint64, slotHashes sealevel.SysvarSlotHashes, reader sealevel.AccountReader) *sealevel.SlotCtx {
	t.Helper()
	slotHashesData := (&slotHashes).MustMarshal()
	bankSysvars, err := sealevel.NewBankSysvars(slot, &accounts.Account{
		Key:      solana.PublicKey(sealevel.SysvarSlotHashesAddr),
		Lamports: 1,
		Data:     slotHashesData,
	})
	require.NoError(t, err)
	slotCtx := &sealevel.SlotCtx{Slot: slot, UnrootedRead: reader}
	require.NoError(t, slotCtx.PublishBankSysvars(bankSysvars))
	return slotCtx
}

func lookupTransaction(table solana.PublicKey, index byte) *solana.Transaction {
	tx := &solana.Transaction{Message: solana.Message{AccountKeys: []solana.PublicKey{{3}}}}
	tx.Message.SetAddressTableLookups([]solana.MessageAddressTableLookup{{
		AccountKey: table, WritableIndexes: solana.Uint8SliceAsNum{index},
	}})
	return tx
}

func TestResolveAddrTableLookupsUsesCapturedBank(t *testing.T) {
	table := solana.PublicKey{1}
	loaded := solana.PublicKey{2}
	reader := &lookupBankReader{
		key:     table,
		account: lookupTableAccount(table, math.MaxUint64, 9, 0, loaded),
	}
	slotCtx := lookupSlotCtx(t, 10, nil, reader)
	tx := lookupTransaction(table, 0)

	require.NoError(t, ResolveAddrTableLookupsForTxInBank(context.Background(), tx, slotCtx))
	require.Equal(t, 1, reader.reads)
	require.Equal(t, loaded, tx.Message.AccountKeys[len(tx.Message.AccountKeys)-1])
}

func TestResolveAddrTableLookupsRequiresPublishedBankSysvars(t *testing.T) {
	table := solana.PublicKey{1}
	tx := lookupTransaction(table, 0)
	err := ResolveAddrTableLookupsForTxInBank(context.Background(), tx, &sealevel.SlotCtx{Slot: 10})
	require.ErrorContains(t, err, "requires SlotHashes")
}

func TestAddressLookupTableRejectsWrongOwner(t *testing.T) {
	table := solana.PublicKey{1}
	acct := lookupTableAccount(table, math.MaxUint64, 9, 0, solana.PublicKey{2})
	acct.Owner = [32]byte{}

	_, err := activeAddressLookupTableAddresses(table, acct, 10, nil)
	require.ErrorContains(t, err, "invalid owner")
	errorType, ok := AddressLookupTableTransactionError(err)
	require.True(t, ok)
	require.Equal(t, TransactionErrorInvalidAddressLookupTableOwner, errorType)
}

func TestAddressLookupTableRejectsFullyDeactivatedTable(t *testing.T) {
	table := solana.PublicKey{1}
	acct := lookupTableAccount(table, 7, 6, 0, solana.PublicKey{2})

	_, err := activeAddressLookupTableAddresses(table, acct, 10, nil)
	require.ErrorContains(t, err, "deactivated")
	errorType, ok := AddressLookupTableTransactionError(err)
	require.True(t, ok)
	require.Equal(t, TransactionErrorAddressLookupTableNotFound, errorType)
}

func TestAddressLookupTableClassifiesInvalidData(t *testing.T) {
	table := solana.PublicKey{1}
	acct := &accounts.Account{Key: table, Data: []byte{1}, Owner: [32]byte(a.AddressLookupTableAddr)}

	_, err := activeAddressLookupTableAddresses(table, acct, 10, nil)
	errorType, ok := AddressLookupTableTransactionError(err)
	require.True(t, ok)
	require.Equal(t, TransactionErrorInvalidAddressLookupTableData, errorType)
}

func TestResolveAddrTableLookupsClassifiesHiddenIndex(t *testing.T) {
	table := solana.PublicKey{1}
	reader := &lookupBankReader{
		key:     table,
		account: lookupTableAccount(table, math.MaxUint64, 10, 1, solana.PublicKey{2}, solana.PublicKey{3}),
	}
	slotCtx := lookupSlotCtx(t, 10, nil, reader)
	tx := lookupTransaction(table, 1)

	err := ResolveAddrTableLookupsForTxInBank(context.Background(), tx, slotCtx)
	errorType, ok := AddressLookupTableTransactionError(err)
	require.True(t, ok)
	require.Equal(t, TransactionErrorInvalidAddressLookupTableIndex, errorType)
}

func TestAddressLookupTableHidesSameSlotExtension(t *testing.T) {
	table := solana.PublicKey{1}
	oldAddress := solana.PublicKey{2}
	newAddress := solana.PublicKey{3}
	acct := lookupTableAccount(table, math.MaxUint64, 10, 1, oldAddress, newAddress)

	addresses, err := activeAddressLookupTableAddresses(table, acct, 10, nil)
	require.NoError(t, err)
	require.Equal(t, solana.PublicKeySlice{oldAddress}, addresses)
}

func TestAddressLookupTableAllowsDeactivatingTable(t *testing.T) {
	table := solana.PublicKey{1}
	loaded := solana.PublicKey{2}
	acct := lookupTableAccount(table, 9, 8, 0, loaded)
	slotHashes := sealevel.SysvarSlotHashes{{Slot: 9}}

	addresses, err := activeAddressLookupTableAddresses(table, acct, 10, slotHashes)
	require.NoError(t, err)
	require.Equal(t, solana.PublicKeySlice{loaded}, addresses)
}

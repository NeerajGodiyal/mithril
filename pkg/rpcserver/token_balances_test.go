package rpcserver

import (
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeTokenAccount builds a 165-byte SPL Token account payload.
func makeTokenAccount(t *testing.T, mint, owner solana.PublicKey, amount uint64) *accounts.Account {
	t.Helper()
	data := make([]byte, tokenAccountSize)
	copy(data[tokenAccountMintOffset:], mint[:])
	copy(data[tokenAccountOwnerOffset:], owner[:])
	binary.LittleEndian.PutUint64(data[tokenAccountAmountOffset:], amount)
	data[tokenAccountStateOffset] = 1
	var ownerArr [32]byte
	copy(ownerArr[:], splTokenProgramID[:])
	return &accounts.Account{Owner: ownerArr, Data: data}
}

func makeTokenMint(programID solana.PublicKey, decimals uint8) *accounts.Account {
	data := make([]byte, mintAccountSize)
	data[mintDecimalsOffset] = decimals
	data[mintInitializedOffset] = 1
	return &accounts.Account{Owner: [32]byte(programID), Data: data}
}

func tokenBalanceTestTx(accountKey, programID solana.PublicKey) *solana.Transaction {
	return &solana.Transaction{Message: solana.Message{AccountKeys: []solana.PublicKey{accountKey, programID}}}
}

func TestTokenBalancesUseCapturedBankMint(t *testing.T) {
	mint := solana.PublicKey{7}
	accountKey := solana.PublicKey{9}
	tokenAcct := makeTokenAccount(t, mint, solana.PublicKey{8}, 1234567)
	tx := tokenBalanceTestTx(accountKey, splTokenProgramID)
	reads := 0

	got := tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, nil}, func(key solana.PublicKey) (*accounts.Account, error) {
		reads++
		require.Equal(t, mint, key)
		return makeTokenMint(splTokenProgramID, 6), nil
	})
	require.Len(t, got, 1)
	assert.Equal(t, "1234567", got[0].UiTokenAmount.Amount)
	assert.Equal(t, uint8(6), got[0].UiTokenAmount.Decimals)
	assert.Equal(t, "1.234567", got[0].UiTokenAmount.UiAmountString)
	assert.Equal(t, 1, reads)
}

func TestTokenBalancesRequireValidInitializedMintAndAccount(t *testing.T) {
	mint := solana.PublicKey{7}
	accountKey := solana.PublicKey{9}
	tokenAcct := makeTokenAccount(t, mint, solana.PublicKey{8}, 123)
	tx := tokenBalanceTestTx(accountKey, splTokenProgramID)

	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, nil}, func(solana.PublicKey) (*accounts.Account, error) {
		return nil, nil
	}))
	wrongOwner := makeTokenMint(splToken2022ProgramID, 6)
	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, nil}, func(solana.PublicKey) (*accounts.Account, error) {
		return wrongOwner, nil
	}))
	uninitializedMint := makeTokenMint(splTokenProgramID, 6)
	uninitializedMint.Data[mintInitializedOffset] = 0
	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, nil}, func(solana.PublicKey) (*accounts.Account, error) {
		return uninitializedMint, nil
	}))

	badState := *tokenAcct
	badState.Data = append([]byte(nil), tokenAcct.Data...)
	badState.Data[tokenAccountStateOffset] = 0
	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{&badState, nil}, func(solana.PublicKey) (*accounts.Account, error) {
		return makeTokenMint(splTokenProgramID, 6), nil
	}))
}

func TestTokenBalancesValidateToken2022Envelope(t *testing.T) {
	mint := solana.PublicKey{7}
	accountKey := solana.PublicKey{9}
	tokenAcct := makeTokenAccount(t, mint, solana.PublicKey{8}, 123)
	tokenAcct.Owner = [32]byte(splToken2022ProgramID)
	tokenAcct.Data = append(tokenAcct.Data, token2022AccountType)
	mintAcct := makeTokenMint(splToken2022ProgramID, 9)
	mintAcct.Data = append(mintAcct.Data, make([]byte, token2022TypeOffset-mintAccountSize)...)
	mintAcct.Data = append(mintAcct.Data, token2022MintType)
	tx := tokenBalanceTestTx(accountKey, splToken2022ProgramID)
	readMint := func(solana.PublicKey) (*accounts.Account, error) { return mintAcct, nil }

	got := tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, nil}, readMint)
	require.Len(t, got, 1)
	assert.Equal(t, splToken2022ProgramID.String(), got[0].ProgramId)

	wrongType := *tokenAcct
	wrongType.Data = append([]byte(nil), tokenAcct.Data...)
	wrongType.Data[token2022TypeOffset] = token2022MintType
	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{&wrongType, nil}, readMint))

	multisigSized := *tokenAcct
	multisigSized.Data = append(append([]byte(nil), tokenAcct.Data[:tokenAccountSize]...), make([]byte, tokenMultisigSize-tokenAccountSize)...)
	assert.Empty(t, tokenBalancesForTransaction(tx, []*accounts.Account{&multisigSized, nil}, readMint))
}

func TestTokenBalancesPreferMintFromSameBankSnapshot(t *testing.T) {
	mint := solana.PublicKey{7}
	accountKey := solana.PublicKey{9}
	tokenAcct := makeTokenAccount(t, mint, solana.PublicKey{8}, 123)
	mintAcct := makeTokenMint(splTokenProgramID, 6)
	tx := &solana.Transaction{Message: solana.Message{AccountKeys: []solana.PublicKey{accountKey, mint, splTokenProgramID}}}

	got := tokenBalancesForTransaction(tx, []*accounts.Account{tokenAcct, mintAcct, nil}, func(solana.PublicKey) (*accounts.Account, error) {
		t.Fatal("captured-bank fallback must not override the transaction snapshot mint")
		return nil, nil
	})
	require.Len(t, got, 1)
	assert.Equal(t, uint8(6), got[0].UiTokenAmount.Decimals)
}

func TestUiAmountStringForRaw_TrimsTrailingZeros(t *testing.T) {
	assert.Equal(t, "1.5", uiAmountStringForRaw(15_000_000, 7))
	assert.Equal(t, "1.234567", uiAmountStringForRaw(1_234_567, 6))
	assert.Equal(t, "1", uiAmountStringForRaw(1_000_000, 6))
	assert.Equal(t, "0.0001", uiAmountStringForRaw(100, 6))
	assert.Equal(t, "0", uiAmountStringForRaw(0, 6))
}

func TestUiAmountStringForRaw_ZeroDecimals(t *testing.T) {
	assert.Equal(t, "12345", uiAmountStringForRaw(12345, 0))
}

// High-decimals (>= 20) cases — Token-2022 doesn't bound decimals at the
// protocol level so the renderer must stay precise even past float64
// representable range. Float-based division saturates at uint64(1e20).
func TestUiAmountStringForRaw_HighDecimalsPrecise(t *testing.T) {
	const maxU64 = uint64(18446744073709551615)
	cases := []struct {
		amount   uint64
		decimals uint8
		want     string
	}{
		{maxU64, 20, "0.18446744073709551615"},
		{maxU64, 25, "0.0000018446744073709551615"},
		{1, 20, "0.00000000000000000001"},
		{0, 20, "0"},
		{0, 0, "0"},
		{1, 0, "1"},
	}
	for _, c := range cases {
		got := uiAmountStringForRaw(c.amount, c.decimals)
		assert.Equal(t, c.want, got, "amount=%d decimals=%d", c.amount, c.decimals)
	}
}

func TestUiAmountStringForRaw_LargeAmountModerateDecimals(t *testing.T) {
	// 9007199254740993 is 2^53 + 1, the first integer not exactly
	// representable in float64. The string path must still be exact.
	assert.Equal(t, "9007199254.740993", uiAmountStringForRaw(9007199254740993, 6))
}

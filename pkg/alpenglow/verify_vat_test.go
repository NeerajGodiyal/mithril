package alpenglow

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestCapValidatorStakeEntriesNoTiebreak(t *testing.T) {
	entries := []ValidatorStake{
		{VoteAccount: testValidatorPubkey(1), Stake: 100},
		{VoteAccount: testValidatorPubkey(2), Stake: 100},
		{VoteAccount: testValidatorPubkey(3), Stake: 100},
	}
	capped := capValidatorStakeEntries(entries, 2)
	require.Len(t, capped, 0)

	entries = []ValidatorStake{
		{VoteAccount: testValidatorPubkey(1), Stake: 300},
		{VoteAccount: testValidatorPubkey(2), Stake: 200},
		{VoteAccount: testValidatorPubkey(3), Stake: 100},
		{VoteAccount: testValidatorPubkey(4), Stake: 100},
	}
	capped = capValidatorStakeEntries(entries, 2)
	require.Len(t, capped, 2)
	require.Equal(t, uint64(300), capped[0].Stake)
	require.Equal(t, uint64(200), capped[1].Stake)
}

func testValidatorPubkey(seed byte) solana.PublicKey {
	key, err := solana.NewRandomPrivateKey()
	if err != nil {
		panic(err)
	}
	key[31] = seed
	return key.PublicKey()
}

package replay

import (
	"encoding/binary"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestRewardEpochDelegatedStakesAddress(t *testing.T) {
	require.Equal(
		t,
		"FEhusMkCSpywBcjYA7j3NwrSKGH7oPmM1BbHZNFtjzkU",
		RewardEpochDelegatedStakesAccountAddr().String(),
	)
}

func TestEncodeRewardEpochDelegatedStakesSorted(t *testing.T) {
	low := solana.PublicKey{31: 1}
	high := solana.PublicKey{0: 1}
	data, err := encodeRewardEpochDelegatedStakes(
		70,
		map[solana.PublicKey]uint64{high: 1, low: 1},
		map[solana.PublicKey]uint64{high: 20, low: 10},
	)
	require.NoError(t, err)
	require.Len(t, data, 16+2*rewardEpochDelegatedStakeRecordLen)
	require.Equal(t, uint64(70), binary.LittleEndian.Uint64(data[:8]))
	require.Equal(t, uint64(2), binary.LittleEndian.Uint64(data[8:16]))
	require.Equal(t, low[:], data[16:48])
	require.Equal(t, uint64(10), binary.LittleEndian.Uint64(data[48:56]))
	require.Equal(t, high[:], data[56:88])
	require.Equal(t, uint64(20), binary.LittleEndian.Uint64(data[88:96]))
	require.Equal(t, uint64(80_016), rewardEpochDelegatedStakesMaxDataLen())
}

func TestCoalesceEpochAccountUpdatesKeepsOriginalParentAndFinalValue(t *testing.T) {
	key := solana.NewWallet().PublicKey()
	other := solana.NewWallet().PublicKey()
	updated, parents := coalesceEpochAccountUpdates(
		[]*accounts.Account{
			{Key: key, Lamports: 90},
			{Key: other, Lamports: 5},
			{Key: key, Lamports: 120},
		},
		[]*accounts.Account{
			{Key: key, Lamports: 100},
			{Key: other, Lamports: 4},
			{Key: key, Lamports: 90},
		},
	)
	require.Len(t, updated, 2)
	require.Len(t, parents, 2)
	require.Equal(t, key, updated[0].Key)
	require.Equal(t, uint64(120), updated[0].Lamports)
	require.Equal(t, uint64(100), parents[0].Lamports)
	require.Equal(t, other, updated[1].Key)
}

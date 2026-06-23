package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestRecentBlockhashesFromState(t *testing.T) {
	parentHash := solana.Hash{1}
	entries := []state.BlockhashEntry{{
		Blockhash:            parentHash.String(),
		LamportsPerSignature: 5000,
	}}
	recent, err := RecentBlockhashesFromState(entries)
	require.NoError(t, err)
	require.Len(t, recent, 1)
	require.Equal(t, [32]byte(parentHash), recent[0].Blockhash)
	require.Equal(t, uint64(5000), recent[0].FeeCalculator.LamportsPerSignature)
}

func TestSeedRecentBlockhashesCache(t *testing.T) {
	prev := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	defer func() { sealevel.SysvarCache.RecentBlockHashes.Sysvar = prev }()

	rbh := sealevel.SysvarRecentBlockhashes{{Blockhash: solana.Hash{1}}}
	SeedRecentBlockhashesCache(rbh)
	require.NotNil(t, sealevel.SysvarCache.RecentBlockHashes.Sysvar)
	require.Equal(t, [32]byte(solana.Hash{1}), (*sealevel.SysvarCache.RecentBlockHashes.Sysvar)[0].Blockhash)

	other := sealevel.SysvarRecentBlockhashes{{Blockhash: solana.Hash{2}}}
	SeedRecentBlockhashesCache(other)
	require.Equal(t, [32]byte(solana.Hash{1}), (*sealevel.SysvarCache.RecentBlockHashes.Sysvar)[0].Blockhash)
}

func TestCloneRecentBlockhashesFromCache(t *testing.T) {
	prev := sealevel.SysvarCache.RecentBlockHashes.Sysvar
	defer func() { sealevel.SysvarCache.RecentBlockHashes.Sysvar = prev }()

	sealevel.SysvarCache.RecentBlockHashes.Sysvar = nil
	_, err := cloneRecentBlockhashesFromCache()
	require.Error(t, err)

	rbh := sealevel.SysvarRecentBlockhashes{{Blockhash: solana.Hash{9}, FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000}}}
	sealevel.SysvarCache.RecentBlockHashes.Sysvar = &rbh

	clone, err := cloneRecentBlockhashesFromCache()
	require.NoError(t, err)
	newHash := solana.Hash{2}
	clone.PushLatest(newHash, 5000)
	require.Equal(t, [32]byte(solana.Hash{9}), (*sealevel.SysvarCache.RecentBlockHashes.Sysvar)[0].Blockhash)
	require.Equal(t, [32]byte(newHash), clone[0].Blockhash)
}

func TestPushLatestPrependsFrozenEntryHash(t *testing.T) {
	parentHash := solana.Hash{1}
	newHash := solana.Hash{2}
	recent := sealevel.SysvarRecentBlockhashes{{
		Blockhash:     parentHash,
		FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: 5000},
	}}

	evicted := recent.PushLatest(newHash, 5000)
	require.Equal(t, [32]byte{}, evicted)
	require.Len(t, recent, 2)
	require.Equal(t, [32]byte(newHash), recent[0].Blockhash)
	require.Equal(t, [32]byte(parentHash), recent[1].Blockhash)
}

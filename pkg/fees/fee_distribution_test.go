package fees

import (
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/epochstakes"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func putTestLeaderVote(t *testing.T, epoch uint64, votePubkey, leader solana.PublicKey) {
	t.Helper()
	global.PutEpochStakesEntry(epoch, votePubkey, 1, &epochstakes.VoteAccount{NodePubkey: leader})
	t.Cleanup(func() { global.ClearEpochStakes(epoch) })
}

func testVoteStateData(t *testing.T, leader, revenueCollector solana.PublicKey) []byte {
	t.Helper()
	var authVoters sealevel.AuthorizedVoters
	authVoters.AuthorizedVoters.Set(114, leader)

	versioned := &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionV4,
		V4: sealevel.VoteState4{
			NodePubkey:                    leader,
			AuthorizedWithdrawer:          leader,
			AuthorizedVoters:              authVoters,
			InflationRewardsCollector:     leader,
			BlockRevenueCollector:         revenueCollector,
			InflationRewardsCommissionBps: 0,
			BlockRevenueCommissionBps:     10000,
		},
	}
	data, err := sealevel.MarshalVersionedVoteState(versioned)
	require.NoError(t, err)
	return data
}

func TestResolveTxFeeCollectorWithoutFeature(t *testing.T) {
	leader := solana.NewWallet().PublicKey()
	slotCtx := &sealevel.SlotCtx{
		Epoch:    114,
		Features: features.NewFeaturesDefault(),
	}
	collector, vote := resolveTxFeeCollector(slotCtx, nil, leader, nil)
	require.Equal(t, leader, collector)
	require.Equal(t, solana.PublicKey{}, vote)
}

func TestResolveTxFeeCollectorUsesBlockRevenueCollector(t *testing.T) {
	leader := solana.NewWallet().PublicKey()
	votePubkey := solana.NewWallet().PublicKey()
	revenueCollector := solana.NewWallet().PublicKey()

	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.CustomCommissionCollector, 0)

	slotCtx := &sealevel.SlotCtx{
		Slot:     6208468,
		Epoch:    114,
		Features: feats,
		Accounts: func() accounts.Accounts {
			accts := accounts.NewMemAccountsWithLen(1)
			require.NoError(t, accts.SetAccountWithoutLock(votePubkey, &accounts.Account{
				Key:   votePubkey,
				Owner: a.VoteProgramAddr,
				Data:  testVoteStateData(t, leader, revenueCollector),
			}))
			return accts
		}(),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),
	}

	putTestLeaderVote(t, 114, votePubkey, leader)

	collector, leaderVote := resolveTxFeeCollector(slotCtx, nil, leader, nil)
	require.Equal(t, revenueCollector, collector)
	require.Equal(t, votePubkey, leaderVote)
}

func TestDistributeTxFeesToBlockRevenueCollector(t *testing.T) {
	leader := solana.NewWallet().PublicKey()
	votePubkey := solana.NewWallet().PublicKey()
	revenueCollector := solana.NewWallet().PublicKey()
	const initialCollectorLamports = uint64(1_000_000)

	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.CustomCommissionCollector, 0)

	slotCtx := &sealevel.SlotCtx{
		Slot:     6208468,
		Epoch:    114,
		Features: feats,
		Accounts: func() accounts.Accounts {
			accts := accounts.NewMemAccountsWithLen(2)
			require.NoError(t, accts.SetAccountWithoutLock(votePubkey, &accounts.Account{
				Key:   votePubkey,
				Owner: a.VoteProgramAddr,
				Data:  testVoteStateData(t, leader, revenueCollector),
			}))
			require.NoError(t, accts.SetAccountWithoutLock(revenueCollector, &accounts.Account{
				Key:       revenueCollector,
				Owner:     a.SystemProgramAddr,
				Lamports:  initialCollectorLamports,
				RentEpoch: 0,
			}))
			require.NoError(t, accts.SetAccountWithoutLock(leader, &accounts.Account{
				Key:       leader,
				Owner:     a.SystemProgramAddr,
				Lamports:  999_980_000,
				RentEpoch: 0,
			}))
			return accts
		}(),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),
	}

	putTestLeaderVote(t, 114, votePubkey, leader)

	feeDist := DistributeTxFeesToSlotLeader(&accountsdb.AccountsDb{}, slotCtx, leader, &TxFeeInfoAccumulator{
		TotalFees:     100,
		ExecutionFees: 100,
	}, nil)
	require.Equal(t, uint64(50), feeDist.LamportsBurnt)
	require.Equal(t, revenueCollector, feeDist.FeeCollector)

	collectorAcct, err := slotCtx.GetAccount(revenueCollector)
	require.NoError(t, err)
	require.Equal(t, initialCollectorLamports+50, collectorAcct.Lamports)

	leaderAcct, err := slotCtx.GetAccount(leader)
	require.NoError(t, err)
	require.Equal(t, uint64(999_980_000), leaderAcct.Lamports)
}

func TestDistributeTxFeesBurnsWhenCollectorOwnerInvalid(t *testing.T) {
	leader := solana.NewWallet().PublicKey()
	votePubkey := solana.NewWallet().PublicKey()
	revenueCollector := solana.NewWallet().PublicKey()

	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.CustomCommissionCollector, 0)

	slotCtx := &sealevel.SlotCtx{
		Slot:     6208468,
		Epoch:    114,
		Features: feats,
		Accounts: func() accounts.Accounts {
			accts := accounts.NewMemAccountsWithLen(2)
			require.NoError(t, accts.SetAccountWithoutLock(votePubkey, &accounts.Account{
				Key:   votePubkey,
				Owner: a.VoteProgramAddr,
				Data:  testVoteStateData(t, leader, revenueCollector),
			}))
			require.NoError(t, accts.SetAccountWithoutLock(revenueCollector, &accounts.Account{
				Key:   revenueCollector,
				Owner: a.VoteProgramAddr,
				Data:  []byte{},
			}))
			return accts
		}(),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),
	}

	putTestLeaderVote(t, 114, votePubkey, leader)

	feeDist := DistributeTxFeesToSlotLeader(&accountsdb.AccountsDb{}, slotCtx, leader, &TxFeeInfoAccumulator{
		TotalFees:     100,
		ExecutionFees: 100,
	}, nil)
	require.Equal(t, uint64(100), feeDist.LamportsBurnt)
	require.True(t, feeDist.FeeCollector.IsZero())
}

func TestDistributeTxFeesUsesParentLoaderWhenCollectorMissingFromWorkingSet(t *testing.T) {
	leader := solana.NewWallet().PublicKey()
	const parentLamports = uint64(925_170_843_840)
	const feesTotal = uint64(9_345_000)

	slotCtx := &sealevel.SlotCtx{
		Slot:          969074,
		ParentSlot:    969073,
		Epoch:         17,
		Features:      features.NewFeaturesDefault(),
		Accounts:      accounts.NewMemAccountsWithLen(1),
		ParentAccts:   accounts.NewMemAccountsWithLen(1),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),
	}

	loadParent := func(pk solana.PublicKey) (*accounts.Account, error) {
		require.Equal(t, leader, pk)
		return &accounts.Account{
			Key:      leader,
			Owner:    a.SystemProgramAddr,
			Lamports: parentLamports,
		}, nil
	}

	feeDist := DistributeTxFeesToSlotLeader(nil, slotCtx, leader, &TxFeeInfoAccumulator{
		TotalFees:     feesTotal,
		ExecutionFees: feesTotal,
	}, loadParent)
	require.Equal(t, feesTotal/2, feeDist.LamportsBurnt)
	require.Equal(t, leader, feeDist.FeeCollector)

	collectorAcct, err := slotCtx.GetAccount(leader)
	require.NoError(t, err)
	require.Equal(t, parentLamports+feesTotal/2, collectorAcct.Lamports)

	parentAcct, err := slotCtx.GetParentAccount(leader)
	require.NoError(t, err)
	require.Equal(t, parentLamports, parentAcct.Lamports)
}

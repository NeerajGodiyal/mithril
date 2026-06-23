package replay

import (
	"encoding/binary"
	"testing"

	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func testVATVotePubkey(index int) solana.PublicKey {
	var key solana.PublicKey
	binary.LittleEndian.PutUint32(key[0:4], uint32(index))
	key[4] = 1
	return key
}

func testVATVoteStateWithBLS(seed byte) *sealevel.VoteStateVersions {
	var node solana.PublicKey
	node[0] = seed + 1
	var bls [48]byte
	bls[0] = seed + 1
	return &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionV4,
		V4: sealevel.VoteState4{
			NodePubkey:          node,
			BlsPubkeyCompressed: &bls,
		},
	}
}

func testVATVoteStateWithoutBLS(seed byte) *sealevel.VoteStateVersions {
	var node solana.PublicKey
	node[0] = seed
	return &sealevel.VoteStateVersions{
		Type: sealevel.VoteStateVersionV4,
		V4: sealevel.VoteState4{
			NodePubkey: node,
		},
	}
}

// Mirrors agave/runtime/tests/vote_account.rs::test_clone_and_filter_for_vat_same_stake_at_border
func TestCapVATCandidatesByStakeSameStakeAtBorder(t *testing.T) {
	const numAccounts = MaxAlpenglowVoteAccounts + 2
	candidates := make([]vatVoteCandidate, 0, numAccounts)
	for index := 0; index < numAccounts; index++ {
		stake := uint64(10)
		if index < MaxAlpenglowVoteAccounts-10 {
			stake = 100 + uint64(index)
		}
		candidates = append(candidates, vatVoteCandidate{
			votePubkey: testVATVotePubkey(index),
			stake:      stake,
		})
	}

	selected := capVATCandidatesByStake(candidates, numAccounts)
	require.Len(t, selected, numAccounts)

	selected = capVATCandidatesByStake(candidates, MaxAlpenglowVoteAccounts)
	require.Len(t, selected, MaxAlpenglowVoteAccounts-10)
}

// Mirrors agave/runtime/tests/vote_account.rs::test_clone_and_filter_for_vat_empty_accounts
func TestCapVATCandidatesByStakeEmptyWhenAllTied(t *testing.T) {
	const numAccounts = 3000
	candidates := make([]vatVoteCandidate, 0, numAccounts)
	for index := 0; index < numAccounts; index++ {
		candidates = append(candidates, vatVoteCandidate{
			votePubkey: testVATVotePubkey(index),
			stake:      100,
		})
	}

	selected := capVATCandidatesByStake(candidates, numAccounts-500)
	require.Empty(t, selected)
}

// Mirrors agave/runtime/tests/vote_account.rs::test_clone_and_filter_for_vat_not_enough_lamports
func TestSelectVATVoteAccountsFiltersInsufficientBalance(t *testing.T) {
	const numValidators = MaxAlpenglowVoteAccounts
	stakes := make(map[solana.PublicKey]uint64, numValidators)
	voteCache := make(map[solana.PublicKey]*sealevel.VoteStateVersions, numValidators)
	lamports := make(map[solana.PublicKey]uint64, numValidators)

	entriesToModify := numValidators / 10
	for index := 0; index < numValidators; index++ {
		pubkey := testVATVotePubkey(index)
		stakes[pubkey] = 1_000_000
		voteCache[pubkey] = testVATVoteStateWithBLS(byte(index % 255))
		if index < entriesToModify {
			lamports[pubkey] = VATToBurnPerEpochLamports - 1
		} else {
			lamports[pubkey] = 10_000_000_000
		}
	}

	filteredStakes, _, _ := selectVATVoteAccounts(stakes, voteCache, lamports, VATToBurnPerEpochLamports, MaxAlpenglowVoteAccounts)
	require.LessOrEqual(t, len(filteredStakes), numValidators-entriesToModify)
}

// Mirrors agave/runtime/tests/vote_account.rs::test_clone_and_filter_for_vat_filters_non_alpenglow
func TestSelectVATVoteAccountsRequiresBLSPubkey(t *testing.T) {
	const numValidators = MaxAlpenglowVoteAccounts + 1000
	stakes := make(map[solana.PublicKey]uint64, numValidators)
	voteCache := make(map[solana.PublicKey]*sealevel.VoteStateVersions, numValidators)
	lamports := make(map[solana.PublicKey]uint64, numValidators)

	for index := 0; index < numValidators; index++ {
		pubkey := testVATVotePubkey(index)
		stakes[pubkey] = 1_000_000
		lamports[pubkey] = 10_000_000_000
		if index < MaxAlpenglowVoteAccounts {
			voteCache[pubkey] = testVATVoteStateWithBLS(byte(index % 255))
		} else {
			voteCache[pubkey] = testVATVoteStateWithoutBLS(byte(index % 255))
		}
	}

	filteredStakes, filteredVoteAccts, _ := selectVATVoteAccounts(
		stakes,
		voteCache,
		lamports,
		1,
		MaxAlpenglowVoteAccounts+500,
	)
	require.Len(t, filteredStakes, MaxAlpenglowVoteAccounts)
	for pubkey := range filteredStakes {
		require.NotNil(t, filteredVoteAccts[pubkey].BlsPubkeyCompressed)
	}

	filteredStakes, _, _ = selectVATVoteAccounts(stakes, voteCache, lamports, 1, MaxAlpenglowVoteAccounts-500)
	require.LessOrEqual(t, len(filteredStakes), MaxAlpenglowVoteAccounts-500)
}

// Mirrors agave/runtime/tests/vote_account.rs::test_clone_and_filter_for_vat_truncates
func TestSelectVATVoteAccountsTruncatesByStake(t *testing.T) {
	const currentLimit = 3000
	stakes := make(map[solana.PublicKey]uint64, currentLimit)
	voteCache := make(map[solana.PublicKey]*sealevel.VoteStateVersions, currentLimit)
	lamports := make(map[solana.PublicKey]uint64, currentLimit)

	for index := 0; index < currentLimit; index++ {
		pubkey := testVATVotePubkey(index)
		stakes[pubkey] = uint64(currentLimit - index)
		voteCache[pubkey] = testVATVoteStateWithBLS(byte(index % 255))
		lamports[pubkey] = 10_000_000_000
	}

	filteredStakes, _, _ := selectVATVoteAccounts(stakes, voteCache, lamports, 1, currentLimit+500)
	require.Len(t, filteredStakes, currentLimit)

	lowerLimit := currentLimit - 1000
	filteredStakes, _, _ = selectVATVoteAccounts(stakes, voteCache, lamports, 1, lowerLimit)
	require.LessOrEqual(t, len(filteredStakes), lowerLimit)

	minStake := ^uint64(0)
	for _, stake := range filteredStakes {
		if stake < minStake {
			minStake = stake
		}
	}
	for pubkey, stake := range stakes {
		if stake < minStake {
			_, ok := filteredStakes[pubkey]
			require.False(t, ok)
		}
	}
}

func TestCapVATCandidatesByStakeNoTiebreak(t *testing.T) {
	candidates := []vatVoteCandidate{
		{votePubkey: testVATVotePubkey(1), stake: 100},
		{votePubkey: testVATVotePubkey(2), stake: 100},
		{votePubkey: testVATVotePubkey(3), stake: 100},
		{votePubkey: testVATVotePubkey(4), stake: 50},
	}

	selected := capVATCandidatesByStake(candidates, 2)
	require.Len(t, selected, 0)

	candidates = []vatVoteCandidate{
		{votePubkey: testVATVotePubkey(1), stake: 300},
		{votePubkey: testVATVotePubkey(2), stake: 200},
		{votePubkey: testVATVotePubkey(3), stake: 100},
		{votePubkey: testVATVotePubkey(4), stake: 100},
	}

	selected = capVATCandidatesByStake(candidates, 2)
	require.Len(t, selected, 2)
	require.Equal(t, uint64(300), selected[0].stake)
	require.Equal(t, uint64(200), selected[1].stake)
}

func TestCapVATCandidatesByStakeUnderLimit(t *testing.T) {
	candidates := []vatVoteCandidate{
		{stake: 10},
		{stake: 20},
	}
	selected := capVATCandidatesByStake(candidates, MaxAlpenglowVoteAccounts)
	require.Len(t, selected, 2)
}

func TestMinimumVoteAccountBalanceForVAT(t *testing.T) {
	rent := sealevel.SysvarRent{LamportsPerUint8Year: 1_000_000_000 / (128 * 365 * 24 * 60 * 60 / 2)}
	require.Equal(t, rent.MinimumBalance(sealevel.VoteStateV3Size), minimumVoteAccountBalanceForVAT(&rent, false))
	require.Equal(t, rent.MinimumBalance(sealevel.VoteStateV3Size)+VATToBurnPerEpochLamports, minimumVoteAccountBalanceForVAT(&rent, true))
}

// Mirrors agave/runtime/src/bank/tests.rs::test_vat_burn_slot_params
func TestDebitVoteAccountForVATAndCreditIncinerator(t *testing.T) {
	votePubkey := testVATVotePubkey(9)
	before := uint64(10 * VATToBurnPerEpochLamports)
	parentVote := &accounts.Account{
		Key:       votePubkey,
		Lamports:  before,
		Owner:     a.VoteProgramAddr,
		RentEpoch: 0,
	}

	newVote, err := debitVoteAccountForVAT(parentVote, VATToBurnPerEpochLamports)
	require.NoError(t, err)
	require.Equal(t, before-VATToBurnPerEpochLamports, newVote.Lamports)

	parentIncinerator := &accounts.Account{Key: a.IncineratorAddr, Lamports: 123}
	newIncinerator := creditIncineratorForVAT(parentIncinerator, VATToBurnPerEpochLamports)
	require.Equal(t, uint64(123)+VATToBurnPerEpochLamports, newIncinerator.Lamports)
}

func TestFilterVoteCacheForVAT(t *testing.T) {
	allowedPubkey := testVATVotePubkey(1)
	droppedPubkey := testVATVotePubkey(2)
	allowed := map[solana.PublicKey]struct{}{allowedPubkey: {}}
	cache := map[solana.PublicKey]*sealevel.VoteStateVersions{
		allowedPubkey: testVATVoteStateWithBLS(1),
		droppedPubkey: testVATVoteStateWithBLS(2),
	}

	filtered := filterVoteCacheForVAT(cache, allowed)
	require.Len(t, filtered, 1)
	require.NotNil(t, filtered[allowedPubkey])
	require.Nil(t, filtered[droppedPubkey])
}

package epochstakes

import (
	"github.com/gagliardetto/solana-go"
)

type EpochStakesCache struct {
	pubkeys         []solana.PublicKey
	stakeCache      map[uint64]map[solana.PublicKey]uint64
	voteAcctCache   map[uint64]map[solana.PublicKey]*VoteAccount
	totalStakeCache map[uint64]uint64
}

type VoteAccount struct {
	Lamports          uint64
	NodePubkey        solana.PublicKey
	LastTimestampTs   int64
	LastTimestampSlot uint64
	Owner             solana.PublicKey
	Executable        byte
	RentEpoch         uint64
}

func NewEpochStakesCache() *EpochStakesCache {
	return &EpochStakesCache{stakeCache: make(map[uint64]map[solana.PublicKey]uint64),
		voteAcctCache:   make(map[uint64]map[solana.PublicKey]*VoteAccount),
		totalStakeCache: make(map[uint64]uint64)}
}

func (cache *EpochStakesCache) PutEntry(epoch uint64, pubkey solana.PublicKey, stake uint64, voteAcct *VoteAccount) {
	_, exists := cache.stakeCache[epoch]
	if !exists {
		cache.stakeCache[epoch] = make(map[solana.PublicKey]uint64)
		cache.voteAcctCache[epoch] = make(map[solana.PublicKey]*VoteAccount)
	}
	cache.stakeCache[epoch][pubkey] = stake
	cache.voteAcctCache[epoch][pubkey] = voteAcct
}

func (cache *EpochStakesCache) PutTotalEpochStake(epoch uint64, totalStake uint64) {
	cache.totalStakeCache[epoch] = totalStake
}

func (cache *EpochStakesCache) EpochStakes(epoch uint64) map[solana.PublicKey]uint64 {
	return cache.stakeCache[epoch]
}

func (cache *EpochStakesCache) HasEpochStakes(epoch uint64) bool {
	_, exists := cache.stakeCache[epoch]
	return exists
}

func (cache *EpochStakesCache) EpochStakesAccts(epoch uint64) map[solana.PublicKey]*VoteAccount {
	return cache.voteAcctCache[epoch]
}

func (cache *EpochStakesCache) TotalStake(epoch uint64) uint64 {
	return cache.totalStakeCache[epoch]
}

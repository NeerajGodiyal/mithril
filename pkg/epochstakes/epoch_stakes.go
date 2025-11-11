package epochstakes

import (
	"github.com/gagliardetto/solana-go"
)

type EpochStakesCache struct {
	pubkeys []solana.PublicKey
	cache   map[uint64]map[solana.PublicKey]uint64
}

func NewEpochStakesCache() *EpochStakesCache {
	return &EpochStakesCache{cache: make(map[uint64]map[solana.PublicKey]uint64)}
}

func (cache *EpochStakesCache) PutEntry(epoch uint64, pubkey solana.PublicKey, stake uint64) {
	_, exists := cache.cache[epoch]
	if !exists {
		cache.cache[epoch] = make(map[solana.PublicKey]uint64)
	}
	cache.cache[epoch][pubkey] = stake
}

func (cache *EpochStakesCache) EpochStakes(epoch uint64) map[solana.PublicKey]uint64 {
	return cache.cache[epoch]
}

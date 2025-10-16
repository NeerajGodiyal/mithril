// This package acts as a singleton for storage and retrieval of data shared between replay and RPC

package global

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

type GlobalCtx struct {
	latestBlockhash     [32]byte
	blockHeight         uint64
	slot                uint64
	epoch               uint64
	transactionCount    uint64
	stakeCache          map[solana.PublicKey]*sealevel.Delegation
	voteCache           map[solana.PublicKey]*sealevel.VoteStateVersions
	slotsConfirmed      map[uint64]struct{}
	stakeCacheMutex     sync.Mutex
	voteCacheMutex      sync.Mutex
	slotsConfirmedMutex sync.Mutex
	mu                  sync.Mutex
}

var instance GlobalCtx

func SetLatestBlockHash(blockHash [32]byte) {
	instance.SetLatestBlockhash(blockHash)
}

func SetBlockHeight(blockHeight uint64) {
	instance.SetBlockHeight(blockHeight)
}

func SetSlot(slot uint64) {
	instance.SetSlot(slot)
}

func SetEpoch(epoch uint64) {
	instance.SetEpoch(epoch)
}

func IncrTransactionCount(num uint64) {
	instance.IncrTransactionCount(num)
}

func PutStakeCacheItem(pubkey solana.PublicKey, delegation *sealevel.Delegation) {
	if instance.stakeCache == nil {
		instance.stakeCache = make(map[solana.PublicKey]*sealevel.Delegation)
	}
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	instance.stakeCache[pubkey] = delegation
}

func DeleteStakeCacheItem(pubkey solana.PublicKey) {
	instance.stakeCacheMutex.Lock()
	defer instance.stakeCacheMutex.Unlock()
	delete(instance.stakeCache, pubkey)
}

func PutSlotConfirmed(slot uint64) {
	if instance.slotsConfirmed == nil {
		instance.slotsConfirmed = make(map[uint64]struct{})
	}
	instance.slotsConfirmedMutex.Lock()
	defer instance.slotsConfirmedMutex.Unlock()
	instance.slotsConfirmed[slot] = struct{}{}
}

func SlotConfirmed(slot uint64) bool {
	instance.slotsConfirmedMutex.Lock()
	defer instance.slotsConfirmedMutex.Unlock()
	_, exists := instance.slotsConfirmed[slot]
	return exists
}

func StakeCache() map[solana.PublicKey]*sealevel.Delegation {
	return instance.stakeCache
}

func PutVoteCacheItem(pubkey solana.PublicKey, voteState *sealevel.VoteStateVersions) {
	if instance.voteCache == nil {
		instance.voteCache = make(map[solana.PublicKey]*sealevel.VoteStateVersions)
	}
	instance.voteCacheMutex.Lock()
	defer instance.voteCacheMutex.Unlock()
	instance.voteCache[pubkey] = voteState
}

func VoteCacheItem(pubkey solana.PublicKey) *sealevel.VoteStateVersions {
	return instance.voteCache[pubkey]
}

func DeleteVoteCacheItem(pubkey solana.PublicKey) {
	instance.voteCacheMutex.Lock()
	defer instance.voteCacheMutex.Unlock()
	delete(instance.voteCache, pubkey)
}

func VoteCache() map[solana.PublicKey]*sealevel.VoteStateVersions {
	return instance.voteCache
}

func LatestBlockHash() [32]byte {
	return instance.LatestBlockhash()
}

func BlockHeight() uint64 {
	return instance.BlockHeight()
}

func Slot() uint64 {
	return instance.Slot()
}

func Epoch() uint64 {
	return instance.Epoch()
}

func TransactionCount() uint64 {
	return instance.TransactionCount()
}

func (globctx *GlobalCtx) SetLatestBlockhash(blockhash [32]byte) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.latestBlockhash = blockhash
}

func (globctx *GlobalCtx) SetBlockHeight(blockHeight uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.blockHeight = blockHeight
}

func (globctx *GlobalCtx) SetSlot(slot uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.slot = slot
}

func (globctx *GlobalCtx) SetEpoch(epoch uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.epoch = epoch
}

func (globctx *GlobalCtx) IncrTransactionCount(num uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.transactionCount += num
}

func (globctx *GlobalCtx) LatestBlockhash() [32]byte {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.latestBlockhash
}

func (globctx *GlobalCtx) BlockHeight() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.blockHeight
}

func (globctx *GlobalCtx) Slot() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.slot
}

func (globctx *GlobalCtx) Epoch() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.epoch
}

func (globctx *GlobalCtx) TransactionCount() uint64 {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	return globctx.transactionCount
}

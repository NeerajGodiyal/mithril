// This package acts as a singleton for storage and retrieval of data shared between replay and RPC

package global

import (
	"sync"
)

type GlobalCtx struct {
	latestBlockhash [32]byte
	blockHeight     uint64
	slot            uint64
	epoch           uint64
	mu              sync.Mutex
}

var instance GlobalCtx

func SetLatestBlockHash(blockHash [32]byte) {
	instance.setLatestBlockhash(blockHash)
}

func SetBlockHeight(blockHeight uint64) {
	instance.setBlockHeight(blockHeight)
}

func SetSlot(slot uint64) {
	instance.setSlot(slot)
}

func SetEpoch(epoch uint64) {
	instance.setEpoch(epoch)
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

func (globctx *GlobalCtx) setLatestBlockhash(blockhash [32]byte) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.latestBlockhash = blockhash
}

func (globctx *GlobalCtx) setBlockHeight(blockHeight uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.blockHeight = blockHeight
}

func (globctx *GlobalCtx) setSlot(slot uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.slot = slot
}

func (globctx *GlobalCtx) setEpoch(epoch uint64) {
	globctx.mu.Lock()
	defer globctx.mu.Unlock()
	globctx.epoch = epoch
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

package bankhash

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func updateAcctsLtHash(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) {
	deltaLtHash := calculateDeltaLtHash(slotCtx, modifiedAccts)
	slotCtx.AcctsLtHash.Add(deltaLtHash)
	if ltDebug {
		dumpPerAcctDeltas(slotCtx, modifiedAccts)
	}
}

// ltDebug (MITHRIL_LTHASH_DEBUG=1) dumps every account's (key, old, new) state per
// slot so two runs can be diffed to the exact diverging account.
var ltDebug = os.Getenv("MITHRIL_LTHASH_DEBUG") == "1"

func dumpPerAcctDeltas(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) {
	for _, a := range dedupeModifiedAccts(modifiedAccts) {
		prev, err := slotCtx.GetParentAccount(a.Key)
		oldSum := "ERR"
		if err == nil {
			var oh lthash.LtHash
			oh.InitWithAcct(prev)
			oldSum = fmt.Sprintf("%x:%d", oh.Checksum(), prev.Lamports)
		}
		var nh lthash.LtHash
		nh.InitWithAcct(a)
		mlog.Log.Infof("LTDBG slot=%d key=%s old=%s new=%x:%d",
			slotCtx.Slot, a.Key, oldSum, nh.Checksum(), a.Lamports)
	}
}

func calculateDeltaLtHash(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) *lthash.LtHash {
	// Dedupe by key (keep the last/newest value): an account that appears more
	// than once (e.g. both rent-collected and modified, or a sysvar collected via
	// multiple paths) would otherwise have its delta counted multiple times,
	// corrupting the cumulative LtHash.
	modifiedAccts = dedupeModifiedAccts(modifiedAccts)
	if len(modifiedAccts) == 0 {
		return &lthash.LtHash{}
	}

	numWorkers := min(32, len(modifiedAccts))

	hashes := make([]*lthash.LtHash, len(modifiedAccts))
	chunkSize := (len(modifiedAccts) + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	for i := range numWorkers {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := workerID * chunkSize
			end := min(start+chunkSize, len(modifiedAccts))

			for j := start; j < end; j++ {
				acct := modifiedAccts[j]
				hashes[j] = calculateSingleDeltaLtHash(slotCtx, acct)
			}
		}(i)
	}
	wg.Wait()

	var deltaHash lthash.LtHash
	for _, h := range hashes {
		deltaHash.Add(h)
	}

	return &deltaHash
}

// dedupeModifiedAccts collapses duplicate keys, keeping each key's last
// occurrence (the newest value) and preserving first-seen order. nil entries are
// dropped. Without this, a key appearing twice would have its LtHash delta
// applied twice.
func dedupeModifiedAccts(modifiedAccts []*accounts.Account) []*accounts.Account {
	if len(modifiedAccts) < 2 {
		return modifiedAccts
	}

	unique := make([]*accounts.Account, 0, len(modifiedAccts))
	seen := make(map[[32]byte]int, len(modifiedAccts))
	for _, acct := range modifiedAccts {
		if acct == nil {
			continue
		}
		key := [32]byte(acct.Key)
		if idx, ok := seen[key]; ok {
			unique[idx] = acct
			continue
		}
		seen[key] = len(unique)
		unique = append(unique, acct)
	}
	return unique
}

func calculateSingleDeltaLtHash(slotCtx *sealevel.SlotCtx, modifiedAcct *accounts.Account) *lthash.LtHash {
	previousAcct, err := slotCtx.GetParentAccount(modifiedAcct.Key)
	if err != nil {
		panic(fmt.Sprintf("couldn't find parent acct for %s for slot %d", modifiedAcct.Key, slotCtx.Slot))
	}

	var deltaLtHash lthash.LtHash

	if previousAcct.Lamports != 0 {
		if acctsEqual(modifiedAcct, previousAcct) {
			return &deltaLtHash
		}

		var oldLtHash lthash.LtHash
		oldLtHash.InitWithAcct(previousAcct)
		deltaLtHash.Sub(&oldLtHash)
	}

	var newLtHash lthash.LtHash
	newLtHash.InitWithAcct(modifiedAcct)
	deltaLtHash.Add(&newLtHash)

	return &deltaLtHash
}

func acctsEqual(a *accounts.Account, b *accounts.Account) bool {
	return a.Lamports == b.Lamports &&
		a.Executable == b.Executable &&
		a.RentEpoch == b.RentEpoch &&
		a.Owner == b.Owner &&
		bytes.Equal(a.Data, b.Data)
}

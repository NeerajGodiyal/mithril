package bankhash

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
)

func updateAcctsLtHash(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) {
	deltaLtHash := calculateDeltaLtHash(slotCtx, modifiedAccts)
	slotCtx.AcctsLtHash.Add(deltaLtHash)
}

func calculateDeltaLtHash(slotCtx *sealevel.SlotCtx, modifiedAccts []*accounts.Account) *lthash.LtHash {
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

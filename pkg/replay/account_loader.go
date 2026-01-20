package replay

import (
	"context"
	"fmt"
	"io"
	"runtime/trace"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/gagliardetto/solana-go"
)

// accountLoader abstracts the loading and storing of accounts for block processing.
//
// Invariant: Every call to NextBlock must be followed by one call to
// StoreAccounts in order for NextBlock to be called again, even for skipped
// blocks or when there are no modified accounts. This enables pipelined
// implementations to synchronize correctly.
type accountLoader interface {
	// NextBlock returns the next block and the accounts it needs for execution.
	// Returns (nil, nil, nil) when there are no more blocks.
	// For skipped blocks, returns (block, nil, nil) since no accounts are needed.
	NextBlock() (*b.Block, map[solana.PublicKey]*accounts.Account, error)

	// StoreAccounts persists the modified accounts to the accounts database.
	// modifiedAccts may be nil or empty for skipped blocks or blocks with no
	// writes. It accepts an optional callback to run after accounts are written
	// to disk, since that may happen after the function call returns.
	StoreAccounts(modifiedAccts []*accounts.Account, slot uint64, afterStoreCallback func()) error

	// Close waits for any outstanding StoreAccounts calls to finish, then
	// returns.
	Close()
}

func callIfNonnil(f func()) {
	if f != nil {
		f()
	}
}

var _ accountLoader = (*sequentialAccountLoader)(nil)

// sequentialAccountLoader implements accountLoader with synchronous loading and storing.
// This preserves the existing behavior where loading blocks execution.
type sequentialAccountLoader struct {
	acctsDb     *accountsdb.AccountsDb
	blockSource *blockstream.BlockSource
}

func (l *sequentialAccountLoader) NextBlock() (*b.Block, map[solana.PublicKey]*accounts.Account, error) {
	block := l.blockSource.NextBlock()
	if block == nil || block.IsSkipped {
		return block, nil, nil
	}

	acctsMap, err := loadBlockAccounts(context.Background(), l.acctsDb, block)
	if err != nil {
		return nil, nil, err
	}
	return block, acctsMap, nil
}

func (l *sequentialAccountLoader) StoreAccounts(modifiedAccts []*accounts.Account, slot uint64, afterStoreCallback func()) error {
	err := l.acctsDb.StoreAccounts(modifiedAccts, slot)
	if err != nil {
		return err
	}
	callIfNonnil(afterStoreCallback)
	return nil
}

func (l *sequentialAccountLoader) Close() {}

var _ accountLoader = (*accountPrefetcher)(nil)

type prefetchResult struct {
	blk   *b.Block
	accts map[solana.PublicKey]*accounts.Account
	err   error
}

type storeRequest struct {
	accts []*accounts.Account
	slot  uint64
	cb    func()
}

// Buffers one block and prefetches accounts for the buffered block.
type accountPrefetcher struct {
	acctsDb     *accountsdb.AccountsDb
	blockSource *blockstream.BlockSource

	prefetch chan prefetchResult
	store    chan storeRequest
}

func newAccountPrefetcher(a *accountsdb.AccountsDb, b *blockstream.BlockSource) *accountPrefetcher {
	p := &accountPrefetcher{
		acctsDb:     a,
		blockSource: b,
		prefetch:    make(chan prefetchResult),
		store:       make(chan storeRequest, 1),
	}
	go p.worker()
	return p
}

func (p *accountPrefetcher) worker() {
	// Intentionally not cancellation context aware. Cancellation should be done in outer loops and call Close.
	ctx := context.Background()
	defer close(p.prefetch)
	// Load first block
	block, acctsMap, err := p.loadBlockAccountsFromDB()
	if err != nil {
		p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: loading initial block: %w", err)}
		return
	}

	var prevStore *storeRequest

	for block != nil {
		// Send block for execution
		p.prefetch <- prefetchResult{block, acctsMap, nil}

		// While that block is executing, try to write previous block's stores
		if prevStore != nil {
			trace.WithRegion(ctx, "PrefetcherStoreAccounts", func() {
				err = p.acctsDb.StoreAccounts(prevStore.accts, prevStore.slot)
			})
			if err != nil {
				p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: storing accounts: %w", err)}
				return
			}
			callIfNonnil(prevStore.cb)
			prevStore = nil
		}

		// then get the next block and see if we can prefetch accounts for it.
		nextBlock := p.blockSource.NextBlock()
		nextNeedsAccounts := nextBlock != nil && !nextBlock.IsSkipped

		var nextAcctsMap map[solana.PublicKey]*accounts.Account
		prefetch := nextNeedsAccounts && !altKeysOverlapWritable(nextBlock, block)
		// These branches must be exhaustive and each branch must receive from
		// p.store due to the accountLoader invariant.
		if prefetch {
			nextAcctsMap, err = loadBlockAccounts(ctx, p.acctsDb, nextBlock)
			if err != nil {
				p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: loading accounts on prefetch path: %w", err)}
				return
			}

			s, ok := <-p.store
			if !ok {
				return
			}

			// patch prefetched accounts with current block's modifications
			for _, modifiedAcct := range s.accts {
				if _, exists := nextAcctsMap[modifiedAcct.Key]; exists {
					nextAcctsMap[modifiedAcct.Key] = modifiedAcct
				}
			}

			prevStore = &s
			acctsMap = nextAcctsMap
		} else if nextNeedsAccounts {
			s, ok := <-p.store
			if !ok {
				return
			}

			mlog.Log.Infof("slow path, prev block wrote my ALT :(")
			trace.WithRegion(ctx, "PrefetcherStoreAccounts", func() {
				err = p.acctsDb.StoreAccounts(s.accts, s.slot)
			})
			if err != nil {
				p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: storing accounts: %w", err)}
				return
			}
			callIfNonnil(s.cb)
			prevStore = nil

			acctsMap, err = loadBlockAccounts(ctx, p.acctsDb, nextBlock)
			if err != nil {
				p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: loading accounts on slow path: %w", err)}
				return
			}
		} else {
			s, ok := <-p.store
			if !ok {
				return
			}

			prevStore = &s
			acctsMap = nil
		}

		block = nextBlock
	}

	// Write final store
	if prevStore != nil {
		err := p.acctsDb.StoreAccounts(prevStore.accts, prevStore.slot)
		if err != nil {
			p.prefetch <- prefetchResult{nil, nil, fmt.Errorf("prefetcher: %w", err)}
			return
		}
		callIfNonnil(prevStore.cb)
	}

	// Signal end of stream
	p.prefetch <- prefetchResult{nil, nil, nil}
}

// altKeysOverlapWritable returns true if any ALT used by nextBlock is writable by block.
func altKeysOverlapWritable(nextBlock, block *b.Block) bool {
	writableKeys := extractWritableKeys(block)
	for _, altKey := range extractALTKeys(nextBlock) {
		if writableKeys[altKey] {
			return true
		}
	}
	return false
}

// loadBlockAccounts resolves ALTs and loads accounts for a block.
func loadBlockAccounts(ctx context.Context, acctsDb *accountsdb.AccountsDb, block *b.Block) (map[solana.PublicKey]*accounts.Account, error) {
	if err := resolveAddrTableLookups(acctsDb, block); err != nil {
		return nil, err
	}

	// Load transaction accounts
	dedupedKeys := extractAndDedupeBlockAccts(block)
	r := trace.StartRegion(ctx, "GetAccountsBatch")
	loadedAccts, err := acctsDb.GetAccountsBatch(ctx, block.Slot, dedupedKeys)
	r.End()
	if err != nil {
		return nil, err
	}

	acctsMap := make(map[solana.PublicKey]*accounts.Account, len(loadedAccts))
	for _, acct := range loadedAccts {
		acctsMap[acct.Key] = acct
	}
	return acctsMap, nil
}

// loadBlockAccountsFromDB fetches the next block and loads its accounts from the database.
func (p *accountPrefetcher) loadBlockAccountsFromDB() (*b.Block, map[solana.PublicKey]*accounts.Account, error) {
	block := p.blockSource.NextBlock()
	if block == nil || block.IsSkipped {
		return block, nil, nil
	}

	acctsMap, err := loadBlockAccounts(context.Background(), p.acctsDb, block)
	if err != nil {
		return nil, nil, err
	}
	return block, acctsMap, nil
}

// extractWritableKeys returns all unique writable account keys from a block.
func extractWritableKeys(block *b.Block) map[solana.PublicKey]bool {
	writable := make(map[solana.PublicKey]bool)
	for _, tx := range block.Transactions {
		ams := mustAccountMetaList(tx)
		for _, am := range ams {
			if am.IsWritable {
				writable[am.PublicKey] = true
			}
		}
	}
	return writable
}

// extractALTKeys returns all unique ALT keys referenced by versioned transactions in the block.
func extractALTKeys(block *b.Block) []solana.PublicKey {
	seen := make(map[solana.PublicKey]bool)
	var keys []solana.PublicKey

	for _, tx := range block.Transactions {
		if !tx.Message.IsVersioned() {
			continue
		}
		for _, altKey := range tx.Message.GetAddressTableLookups().GetTableIDs() {
			if !seen[altKey] {
				seen[altKey] = true
				keys = append(keys, altKey)
			}
		}
	}
	return keys
}

func (p *accountPrefetcher) NextBlock() (*b.Block, map[solana.PublicKey]*accounts.Account, error) {
	r, ok := <-p.prefetch
	if !ok {
		return nil, nil, io.EOF
	}
	return r.blk, r.accts, r.err
}

func (p *accountPrefetcher) StoreAccounts(modifiedAccounts []*accounts.Account, slot uint64, afterStoreCallback func()) error {
	p.store <- storeRequest{modifiedAccounts, slot, afterStoreCallback}
	return nil
}

func (p *accountPrefetcher) Close() {
	close(p.store)
	// Consume any buffered blocks and wait for worker's defer
	// close(p.prefetch) to know it's terminated.
	for range p.prefetch {
	}
}

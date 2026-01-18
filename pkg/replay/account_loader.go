package replay

import (
	"context"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/blockstream"
	"github.com/gagliardetto/solana-go"
)

// accountLoader abstracts the loading and storing of accounts for block processing.
// This enables future pipelining where account loading can happen ahead of execution.
type accountLoader interface {
	// NextBlock returns the next block and the accounts it needs for execution.
	// Returns (nil, nil, nil) when there are no more blocks.
	// For skipped blocks, returns (block, nil, nil) since no accounts are needed.
	NextBlock() (*b.Block, map[solana.PublicKey]*accounts.Account, error)

	// StoreAccounts persists the modified accounts to the accounts database.
	StoreAccounts(modifiedAccts []*accounts.Account, slot uint64) error
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
	if block == nil {
		return nil, nil, nil
	}

	if block.IsSkipped {
		return block, nil, nil
	}

	// Resolve address table lookups before extracting account keys
	err := resolveAddrTableLookups(l.acctsDb, block)
	if err != nil {
		return nil, nil, err
	}

	// Extract all unique account keys needed for this block
	dedupedKeys := extractAndDedupeBlockAccts(block)

	// Load accounts from the database
	ctx := context.Background()
	loadedAccts, err := l.acctsDb.GetAccountsBatch(ctx, block.Slot, dedupedKeys)
	if err != nil {
		return nil, nil, err
	}

	// Build the accounts map
	acctsMap := make(map[solana.PublicKey]*accounts.Account, len(loadedAccts))
	for _, acct := range loadedAccts {
		acctsMap[acct.Key] = acct
	}

	return block, acctsMap, nil
}

func (l *sequentialAccountLoader) StoreAccounts(modifiedAccts []*accounts.Account, slot uint64) error {
	if len(modifiedAccts) == 0 {
		return nil
	}
	return l.acctsDb.StoreAccounts(modifiedAccts, slot)
}

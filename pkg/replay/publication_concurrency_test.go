package replay

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func publicationConcurrencyTestKey(n uint64) solana.PublicKey {
	var key solana.PublicKey
	binary.LittleEndian.PutUint64(key[:8], n)
	binary.LittleEndian.PutUint64(key[8:16], n*0x9e3779b97f4a7c15)
	return key
}

func TestConcurrentDisjointTransactionPublication(t *testing.T) {
	const (
		workers          = 16
		batchesPerWorker = 128
		accountsPerBatch = 2
	)
	totalAccounts := workers * batchesPerWorker * accountsPerBatch
	feats := features.NewFeaturesDefault()
	feats.EnableFeature(features.RemoveAccountsDeltaHash, 0)

	parent := accounts.NewMemAccounts()
	overlay := accounts.NewOverlayAccountsWithLen(parent, totalAccounts)
	slotCtx := &sealevel.SlotCtx{
		Accounts:                  overlay,
		Features:                  feats,
		AcctMapsMu:                &sync.Mutex{},
		ModifiedAccts:             make(map[solana.PublicKey]bool),
		WritableAccts:             make(map[solana.PublicKey]bool),
		ModifiedAccountsFromDelta: true,
	}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func(worker int) {
			defer wg.Done()
			<-start
			for batchIdx := range batchesPerWorker {
				first := uint64((worker*batchesPerWorker+batchIdx)*accountsPerBatch + 1)
				accountStates := []*accounts.Account{
					{Key: publicationConcurrencyTestKey(first), Lamports: first},
					{Key: publicationConcurrencyTestKey(first + 1), Lamports: first + 1},
				}
				execCtx := &sealevel.ExecutionCtx{
					Features: *feats,
					TransactionContext: &sealevel.TransactionCtx{
						Accounts: sealevel.TransactionAccounts{
							Accounts: accountStates,
							Touched:  []bool{true, true},
						},
					},
				}
				if err := applySuccessfulTransactionState(slotCtx, execCtx, nil); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	assert.Empty(t, slotCtx.WritableAccts, "ADH-removed replay must not build the unused writable set")
	assert.Empty(t, slotCtx.ModifiedAccts, "overlay delta replaces the contended modified-account map")
	require.Len(t, overlay.DeltaAccounts(), totalAccounts)
	for _, modified := range overlay.DeltaAccounts() {
		key := modified.Key
		acct, err := overlay.GetAccount((*[32]byte)(&key))
		require.NoError(t, err)
		assert.Equal(t, binary.LittleEndian.Uint64(key[:8]), acct.Lamports)
	}
}

func TestConcurrentDisjointLegacyTransactionPublication(t *testing.T) {
	const (
		workers          = 8
		batchesPerWorker = 64
		accountsPerBatch = 2
	)
	totalAccounts := workers * batchesPerWorker * accountsPerBatch
	feats := features.NewFeaturesDefault()

	parent := accounts.NewMemAccounts()
	overlay := accounts.NewOverlayAccountsWithLen(parent, totalAccounts)
	slotCtx := &sealevel.SlotCtx{
		Accounts:      overlay,
		Features:      feats,
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool, totalAccounts),
		WritableAccts: make(map[solana.PublicKey]bool, totalAccounts),
	}

	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func(worker int) {
			defer wg.Done()
			<-start
			for batchIdx := range batchesPerWorker {
				first := uint64((worker*batchesPerWorker+batchIdx)*accountsPerBatch + 1)
				firstKey := publicationConcurrencyTestKey(first)
				secondKey := publicationConcurrencyTestKey(first + 1)
				accountStates := []*accounts.Account{
					{Key: firstKey, Lamports: first},
					{Key: secondKey, Lamports: first + 1},
				}
				execCtx := &sealevel.ExecutionCtx{
					Features: *feats,
					TransactionContext: &sealevel.TransactionCtx{
						Accounts: sealevel.TransactionAccounts{
							Accounts: accountStates,
							Touched:  []bool{true, true},
						},
					},
				}
				executionResult := &TransactionExecutionResult{
					WritableAccounts: []solana.PublicKey{firstKey, secondKey},
					WritableAccountSet: map[solana.PublicKey]struct{}{
						firstKey:  {},
						secondKey: {},
					},
				}
				if err := applySuccessfulTransactionState(slotCtx, execCtx, executionResult); err != nil {
					errs <- err
					return
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	require.Len(t, slotCtx.WritableAccts, totalAccounts)
	require.Len(t, slotCtx.ModifiedAccts, totalAccounts)
	require.Len(t, overlay.DeltaAccounts(), totalAccounts)
	for key := range slotCtx.ModifiedAccts {
		acct, err := overlay.GetAccount((*[32]byte)(&key))
		require.NoError(t, err)
		assert.Equal(t, binary.LittleEndian.Uint64(key[:8]), acct.Lamports)
	}
}

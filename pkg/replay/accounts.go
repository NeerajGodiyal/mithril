package replay

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

type accountSourceError struct{ err error }

func (e *accountSourceError) Error() string { return e.err.Error() }
func (e *accountSourceError) Unwrap() error { return e.err }

func newAccountSourceError(context string, err error) error {
	return &accountSourceError{err: fmt.Errorf("%s: %w", context, err)}
}

type loadedAccountsSizeError struct {
	size uint32
	err  error
}

func (e *loadedAccountsSizeError) Error() string { return e.err.Error() }
func (e *loadedAccountsSizeError) Unwrap() error { return e.err }

func loadedAccountsSizeOnError(err error) uint32 {
	var sizeErr *loadedAccountsSizeError
	if errors.As(err, &sizeErr) {
		return sizeErr.size
	}
	return 0
}

func loadedAccountsLimitError(size, limit uint64) error {
	if size > limit {
		size = limit
	}
	if size > math.MaxUint32 {
		size = math.MaxUint32
	}
	return &loadedAccountsSizeError{size: uint32(size), err: TxErrMaxLoadedAccountsDataSizeExceeded}
}

// Account clone tracking for profiling copy-on-write optimization potential
var (
	// Per-transaction account load stats (accounts referenced by tx execution)
	TxAcctsLoaded      atomic.Uint64 // Total accounts loaded into tx contexts
	TxAcctsLoadedBytes atomic.Uint64 // Total bytes referenced by tx contexts

	// Per-transaction copy-on-write clone stats (first write in TransactionAccounts.Touch)
	TxAcctsCloned      atomic.Uint64 // Total accounts cloned on first write
	TxAcctsClonedBytes atomic.Uint64 // Total bytes cloned on first write

	// Per-transaction modification stats (touched in handleModifiedAccounts)
	TxAcctsTouched      atomic.Uint64 // Total accounts actually modified
	TxAcctsTouchedBytes atomic.Uint64 // Total bytes of modified account data

	// Transaction count for averaging
	TxCount atomic.Uint64
)

// CloneStats holds account clone/modify metrics for reporting
type CloneStats struct {
	AcctsLoaded       uint64 // Accounts loaded into tx contexts
	AcctsLoadedBytes  uint64 // Bytes referenced by tx contexts
	AcctsCloned       uint64 // Accounts loaded (cloned)
	AcctsClonedBytes  uint64 // Bytes cloned
	AcctsTouched      uint64 // Accounts modified
	AcctsTouchedBytes uint64 // Bytes of modified accounts
	TxCount           uint64 // Number of transactions
}

// GetAndResetCloneStats returns current clone stats and resets counters
func GetAndResetCloneStats() CloneStats {
	return CloneStats{
		AcctsLoaded:       TxAcctsLoaded.Swap(0),
		AcctsLoadedBytes:  TxAcctsLoadedBytes.Swap(0),
		AcctsCloned:       TxAcctsCloned.Swap(0),
		AcctsClonedBytes:  TxAcctsClonedBytes.Swap(0),
		AcctsTouched:      TxAcctsTouched.Swap(0),
		AcctsTouchedBytes: TxAcctsTouchedBytes.Swap(0),
		TxCount:           TxCount.Swap(0),
	}
}

func recordTxAcctCowClone(acct *accounts.Account) {
	TxAcctsCloned.Add(1)
	TxAcctsClonedBytes.Add(uint64(len(acct.Data)))
}

func loadAndValidateTxAccts(slotCtx *sealevel.SlotCtx, txAcctMetas []*solana.AccountMeta, tx *solana.Transaction, instrs []sealevel.Instruction, instrsAcct *accounts.Account, loadedAcctBytesLimit uint32) (*sealevel.TransactionAccounts, []*solana.AccountMeta, error) {
	if txAcctMetas == nil {
		var err error
		txAcctMetas, err = tx.AccountMetaList()
		if err != nil {
			return nil, nil, err
		}
	}

	var programIdIdxs []uint64
	instructionAcctPubkeys := make(map[solana.PublicKey]struct{}, len(tx.Message.AccountKeys))

	for instrIdx, instr := range tx.Message.Instructions {
		programIdIdxs = append(programIdIdxs, uint64(instr.ProgramIDIndex))
		ias := instrs[instrIdx].Accounts
		for _, ia := range ias {
			instructionAcctPubkeys[ia.Pubkey] = struct{}{}
		}
	}

	acctsForTx := make([]*accounts.Account, 0, len(txAcctMetas))
	acctsShared := make([]bool, 0, len(txAcctMetas))
	convertedAcctMetas := make([]*sealevel.AccountMeta, 0, len(txAcctMetas))
	var loadedBytesAccumulator uint32
	var loadedAcctCount uint64
	var loadedAcctBytes uint64
	var err error

	for idx, acctMeta := range txAcctMetas {
		var acct *accounts.Account
		var isInstructionsSysvarAcct bool
		var isSharedAcct bool

		_, instrContainsAcctMeta := instructionAcctPubkeys[acctMeta.PublicKey]
		if acctMeta.PublicKey == sealevel.SysvarInstructionsAddr {
			acct = instrsAcct
			isInstructionsSysvarAcct = true
		} else if !slotCtx.Features.IsActive(features.DisableAccountLoaderSpecialCase) && slices.Contains(programIdIdxs, uint64(idx)) && !acctMeta.IsWritable && !instrContainsAcctMeta {
			tmp, err := slotCtx.GetAccountShared(acctMeta.PublicKey)
			if err != nil {
				return nil, nil, err
			}
			acct = &accounts.Account{Key: acctMeta.PublicKey, Owner: tmp.Owner, Executable: true, IsDummy: true}
		} else {
			acct, err = slotCtx.GetAccountShared(acctMeta.PublicKey)
			if err != nil {
				return nil, nil, err
			}
			isSharedAcct = true
		}

		if !isInstructionsSysvarAcct {
			loadedBytesAccumulator = safemath.SaturatingAddU32(loadedBytesAccumulator, uint32(len(acct.Data)))
			if loadedBytesAccumulator > loadedAcctBytesLimit {
				return nil, nil, loadedAccountsLimitError(uint64(loadedBytesAccumulator), uint64(loadedAcctBytesLimit))
			}
		}

		acctsForTx = append(acctsForTx, acct)
		acctsShared = append(acctsShared, isSharedAcct)
		convertedAcctMeta := &sealevel.AccountMeta{Pubkey: acctMeta.PublicKey, IsSigner: acctMeta.IsSigner, IsWritable: acctMeta.IsWritable}
		convertedAcctMetas = append(convertedAcctMetas, convertedAcctMeta)
		if isSharedAcct {
			loadedAcctCount++
			loadedAcctBytes += uint64(len(acct.Data))
		}
	}

	transactionAccts := sealevel.NewTransactionAccountsFromRefs(acctsForTx, acctsShared)
	transactionAccts.AcctMetas = convertedAcctMetas
	transactionAccts.OnFirstWriteClone = recordTxAcctCowClone
	TxAcctsLoaded.Add(loadedAcctCount)
	TxAcctsLoadedBytes.Add(loadedAcctBytes)

	removeAcctsExecutableFlagChecks := slotCtx.Features.IsActive(features.RemoveAccountsExecutableFlagChecks)
	validatedLoaders := make(map[solana.PublicKey]struct{}, 4) // Usually ≤4 loaders

	for _, instr := range instrs {
		if instr.ProgramId == addresses.NativeLoaderAddr {
			continue
		}

		programAcct, err := slotCtx.GetAccountShared(instr.ProgramId)
		if err != nil {
			return transactionAccts, txAcctMetas, TxErrProgramAccountNotFound
		}

		if programAcct.Lamports == 0 {
			return transactionAccts, txAcctMetas, TxErrProgramAccountNotFound
		}

		if !removeAcctsExecutableFlagChecks && !programAcct.Executable {
			return transactionAccts, txAcctMetas, TxErrInvalidProgramForExecution
		}

		owner := programAcct.Owner
		if owner == addresses.NativeLoaderAddr {
			continue
		}

		_, exists := validatedLoaders[owner]
		if !exists {
			var ownerAcct *accounts.Account
			ownerAcct, err = slotCtx.GetAccountShared(owner)
			if err != nil {
				ownerAcct, err = slotCtx.GetAccountFromAccountsDb(owner)
				if err != nil {
					if !errors.Is(err, accountsdb.ErrNoAccount) {
						return transactionAccts, txAcctMetas, newAccountSourceError("load program owner", err)
					}
					return transactionAccts, txAcctMetas, TxErrInvalidProgramForExecution
				}
			}

			if ownerAcct.Owner != addresses.NativeLoaderAddr || (!removeAcctsExecutableFlagChecks && !ownerAcct.Executable) {
				return transactionAccts, txAcctMetas, TxErrInvalidProgramForExecution
			}

			loadedBytesAccumulator = safemath.SaturatingAddU32(loadedBytesAccumulator, uint32(len(ownerAcct.Data)))
			if loadedBytesAccumulator > loadedAcctBytesLimit {
				transactionAccts.LoadedAccountsDataSize = loadedBytesAccumulator
				return transactionAccts, txAcctMetas, loadedAccountsLimitError(uint64(loadedBytesAccumulator), uint64(loadedAcctBytesLimit))
			}

			validatedLoaders[owner] = struct{}{}
		}
	}
	transactionAccts.LoadedAccountsDataSize = loadedBytesAccumulator

	return transactionAccts, txAcctMetas, nil
}

const (
	txAcctBaseSize          = 64
	addrLookupTableBaseSize = 8248
)

type loadedAcctSizeAccumulatorSimd186 struct {
	limit                 uint64
	accumulator           uint64
	acctKeys              []solana.PublicKey
	additionalLoadedAccts map[solana.PublicKey]struct{}
	slotCtx               *sealevel.SlotCtx
}

func NewLoadedAcctSizeAccumulatorSimd186(
	slotCtx *sealevel.SlotCtx,
	limit uint64,
	acctKeys []solana.PublicKey) *loadedAcctSizeAccumulatorSimd186 {
	return &loadedAcctSizeAccumulatorSimd186{
		limit:                 limit,
		acctKeys:              acctKeys,
		additionalLoadedAccts: make(map[solana.PublicKey]struct{}),
		slotCtx:               slotCtx}
}

func (accum *loadedAcctSizeAccumulatorSimd186) wasAlreadyCounted(pubkey solana.PublicKey) bool {
	for _, addr := range accum.acctKeys {
		if addr == pubkey {
			return true
		}
	}

	_, exists := accum.additionalLoadedAccts[pubkey]
	return exists
}

func (accum *loadedAcctSizeAccumulatorSimd186) add(amount uint64) error {
	accum.accumulator = safemath.SaturatingAddU64(accum.accumulator, amount)
	if accum.accumulator > accum.limit {
		return TxErrMaxLoadedAccountsDataSizeExceeded
	}
	return nil
}

func (accum *loadedAcctSizeAccumulatorSimd186) collectAcct(acct *accounts.Account) error {
	if acct.Key == sealevel.SysvarInstructionsAddr || acct.Lamports == 0 {
		return nil
	}

	acctLen := uint64(len(acct.Data))
	accum.accumulator = safemath.SaturatingAddU64(accum.accumulator, safemath.SaturatingAddU64(acctLen, txAcctBaseSize))
	if accum.accumulator > accum.limit {
		return TxErrMaxLoadedAccountsDataSizeExceeded
	}

	if acct.Owner == addresses.BpfLoaderUpgradeableAddr {
		acctState, err := sealevel.UnmarshalUpgradeableLoaderState(acct.Data)
		if err == nil && acctState != nil && acctState.Type == sealevel.UpgradeableLoaderStateTypeProgram {
			programDataAddr := acctState.Program.ProgramDataAddress
			if !accum.wasAlreadyCounted(programDataAddr) {
				// program data account not being found is not an error. Agave instead ignores it.
				programDataAcct, err := accum.slotCtx.GetAccountShared(programDataAddr)
				if err != nil {
					programDataAcct, err = accum.slotCtx.GetAccountFromAccountsDb(programDataAddr)
					if err != nil {
						if !errors.Is(err, accountsdb.ErrNoAccount) {
							return newAccountSourceError("load program data", err)
						}
						return nil
					}
				}
				accum.accumulator = safemath.SaturatingAddU64(accum.accumulator, safemath.SaturatingAddU64(txAcctBaseSize, uint64(len(programDataAcct.Data))))
				if accum.accumulator > accum.limit {
					return TxErrMaxLoadedAccountsDataSizeExceeded
				}
				accum.additionalLoadedAccts[programDataAddr] = struct{}{}
			}
		}
	}

	return nil
}

func isLoaderAcct(owner solana.PublicKey) bool {
	return owner == addresses.BpfLoaderUpgradeableAddr ||
		owner == addresses.BpfLoader2Addr ||
		owner == addresses.BpfLoaderDeprecatedAddr ||
		owner == addresses.LoaderV4Addr
}

func loadAndValidateTxAcctsSimd186(slotCtx *sealevel.SlotCtx, txAcctMetas []*solana.AccountMeta, tx *solana.Transaction, instrs []sealevel.Instruction, instrsAcct *accounts.Account, loadedAcctBytesLimit uint32) (*sealevel.TransactionAccounts, []*solana.AccountMeta, error) {
	if txAcctMetas == nil {
		var err error
		txAcctMetas, err = tx.AccountMetaList()
		if err != nil {
			return nil, nil, err
		}
	}

	acctKeys := tx.Message.AccountKeys
	accumulator := NewLoadedAcctSizeAccumulatorSimd186(slotCtx,
		uint64(loadedAcctBytesLimit),
		acctKeys)

	addrTableLookupCost := safemath.SaturatingMulU64(uint64(len(tx.Message.AddressTableLookups)), addrLookupTableBaseSize)
	err := accumulator.add(addrTableLookupCost)
	if err != nil {
		return nil, nil, loadedAccountsLimitError(accumulator.accumulator, accumulator.limit)
	}

	// Memoize accounts loaded in Pass 1
	// Use slice indexed by account position (same ordering as txAcctMetas)
	acctCache := make([]*accounts.Account, len(acctKeys))

	for i, pubkey := range acctKeys {
		var acct *accounts.Account
		if pubkey == sealevel.SysvarInstructionsAddr {
			acct = instrsAcct
		} else {
			acct, err = slotCtx.GetAccountShared(pubkey)
			if err != nil && (slotCtx.AccountsDb != nil || slotCtx.UnrootedRead != nil) {
				// Fall back to full accountsdb so native programs
				// (System, BPF Loader, etc.) and other always-on
				// accounts are loaded even when the per-slot
				// MemAccounts didn't reference them.
				acct, err = slotCtx.GetAccountFromAccountsDb(pubkey)
				if err != nil && !errors.Is(err, accountsdb.ErrNoAccount) {
					return nil, nil, newAccountSourceError("load transaction account", err)
				}
			}
			if err != nil {
				// Empty default for genuinely absent pubkeys; matches
				// Agave load_transaction_account (rent_epoch = SIMD-0267).
				acct = &accounts.Account{
					Key:       pubkey,
					Owner:     addresses.SystemProgramAddr,
					RentEpoch: math.MaxUint64,
				}
			}
		}
		acctCache[i] = acct // Cache by index for reuse in Pass 2
		err = accumulator.collectAcct(acct)
		if err != nil {
			var sourceErr *accountSourceError
			if errors.As(err, &sourceErr) {
				return nil, nil, sourceErr
			}
			return nil, nil, loadedAccountsLimitError(accumulator.accumulator, accumulator.limit)
		}
	}

	// Use boolean mask for O(1) program index lookup
	isProgramIdx := make([]bool, len(acctKeys))
	instructionAcctPubkeys := make(map[solana.PublicKey]struct{}, len(acctKeys))

	for instrIdx, instr := range tx.Message.Instructions {
		i := int(instr.ProgramIDIndex)
		if i >= 0 && i < len(isProgramIdx) {
			isProgramIdx[i] = true
		}
		ias := instrs[instrIdx].Accounts
		for _, ia := range ias {
			instructionAcctPubkeys[ia.Pubkey] = struct{}{}
		}
	}

	acctsForTx := make([]*accounts.Account, 0, len(txAcctMetas))
	acctsShared := make([]bool, 0, len(txAcctMetas))
	convertedAcctMetas := make([]*sealevel.AccountMeta, 0, len(txAcctMetas))
	var loadedAcctCount uint64
	var loadedAcctBytes uint64

	for idx, acctMeta := range txAcctMetas {
		var acct *accounts.Account
		var isSharedAcct bool
		cached := acctCache[idx] // Reuse account from Pass 1

		_, instrContainsAcctMeta := instructionAcctPubkeys[acctMeta.PublicKey]
		if acctMeta.PublicKey == sealevel.SysvarInstructionsAddr {
			acct = instrsAcct
		} else if !slotCtx.Features.IsActive(features.DisableAccountLoaderSpecialCase) && isProgramIdx[idx] && !acctMeta.IsWritable && !instrContainsAcctMeta {
			// Dummy account case - only need owner from cached account
			acct = &accounts.Account{Key: acctMeta.PublicKey, Owner: cached.Owner, Executable: true, IsDummy: true}
		} else {
			// Normal case - use cached account directly
			acct = cached
			isSharedAcct = true
		}

		acctsForTx = append(acctsForTx, acct)
		acctsShared = append(acctsShared, isSharedAcct)
		convertedAcctMeta := &sealevel.AccountMeta{Pubkey: acctMeta.PublicKey, IsSigner: acctMeta.IsSigner, IsWritable: acctMeta.IsWritable}
		convertedAcctMetas = append(convertedAcctMetas, convertedAcctMeta)
		if isSharedAcct {
			loadedAcctCount++
			loadedAcctBytes += uint64(len(acct.Data))
		}
	}

	transactionAccts := sealevel.NewTransactionAccountsFromRefs(acctsForTx, acctsShared)
	transactionAccts.AcctMetas = convertedAcctMetas
	transactionAccts.LoadedAccountsDataSize = uint32(accumulator.accumulator)
	transactionAccts.OnFirstWriteClone = recordTxAcctCowClone
	TxAcctsLoaded.Add(loadedAcctCount)
	TxAcctsLoadedBytes.Add(loadedAcctBytes)

	removeAcctsExecutableFlagChecks := slotCtx.Features.IsActive(features.RemoveAccountsExecutableFlagChecks)

	for instrIdx, instr := range instrs {
		if instr.ProgramId == addresses.NativeLoaderAddr {
			continue
		}

		// Use cached account via ProgramIDIndex from tx.Message
		programIdx := int(tx.Message.Instructions[instrIdx].ProgramIDIndex)
		var programAcct *accounts.Account
		if programIdx >= 0 && programIdx < len(acctCache) {
			programAcct = acctCache[programIdx]
		}

		// Fallback if not in cache or out of bounds
		if programAcct == nil {
			var err error
			programAcct, err = slotCtx.GetAccountShared(instr.ProgramId)
			if err != nil {
				programAcct, err = slotCtx.GetAccountFromAccountsDb(instr.ProgramId)
				if err != nil {
					if !errors.Is(err, accountsdb.ErrNoAccount) {
						return transactionAccts, txAcctMetas, newAccountSourceError("load program account", err)
					}
					return transactionAccts, txAcctMetas, TxErrProgramAccountNotFound
				}
			}
		}

		if programAcct.Lamports == 0 {
			return transactionAccts, txAcctMetas, TxErrProgramAccountNotFound
		}

		if !removeAcctsExecutableFlagChecks && !programAcct.Executable {
			return transactionAccts, txAcctMetas, TxErrInvalidProgramForExecution
		}

		owner := programAcct.Owner
		if owner != addresses.NativeLoaderAddr && !isLoaderAcct(owner) {
			return transactionAccts, txAcctMetas, TxErrInvalidProgramForExecution
		}
	}

	TxCount.Add(1)
	return transactionAccts, txAcctMetas, nil
}

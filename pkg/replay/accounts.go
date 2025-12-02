package replay

import (
	"slices"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/addresses"
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

func loadAndValidateTxAccts(slotCtx *sealevel.SlotCtx, acctMetasPerInstr [][]sealevel.AccountMeta, tx *solana.Transaction, instrs []sealevel.Instruction, instrsAcct *accounts.Account, loadedAcctBytesLimit uint32) (*sealevel.TransactionAccounts, error) {
	txAcctMetas, err := tx.AccountMetaList()
	if err != nil {
		return nil, err
	}

	var programIdIdxs []uint64
	instructionAcctPubkeys := make(map[solana.PublicKey]struct{})

	for instrIdx, instr := range tx.Message.Instructions {
		programIdIdxs = append(programIdIdxs, uint64(instr.ProgramIDIndex))
		ias := acctMetasPerInstr[instrIdx]
		for _, ia := range ias {
			instructionAcctPubkeys[ia.Pubkey] = struct{}{}
		}
	}

	acctsForTx := make([]accounts.Account, 0, len(txAcctMetas))
	convertedAcctMetas := make([]*sealevel.AccountMeta, 0, len(txAcctMetas))
	var loadedBytesAccumulator uint32

	for idx, acctMeta := range txAcctMetas {
		var acct *accounts.Account
		var isInstructionsSysvarAcct bool

		_, instrContainsAcctMeta := instructionAcctPubkeys[acctMeta.PublicKey]
		if acctMeta.PublicKey == sealevel.SysvarInstructionsAddr {
			acct = instrsAcct
			isInstructionsSysvarAcct = true
		} else if !slotCtx.Features.IsActive(features.DisableAccountLoaderSpecialCase) && slices.Contains(programIdIdxs, uint64(idx)) && !acctMeta.IsWritable && !instrContainsAcctMeta {
			tmp, err := slotCtx.GetAccount(acctMeta.PublicKey)
			if err != nil {
				return nil, err
			}
			acct = &accounts.Account{Key: acctMeta.PublicKey, Owner: tmp.Owner, Executable: true, IsDummy: true}
		} else {
			acct, err = slotCtx.GetAccount(acctMeta.PublicKey)
			if err != nil {
				return nil, err
			}
		}

		if !isInstructionsSysvarAcct {
			loadedBytesAccumulator = safemath.SaturatingAddU32(loadedBytesAccumulator, uint32(len(acct.Data)))
			if loadedBytesAccumulator > loadedAcctBytesLimit {
				return nil, TxErrMaxLoadedAccountsDataSizeExceeded
			}
		}

		acctsForTx = append(acctsForTx, *acct)
		convertedAcctMeta := &sealevel.AccountMeta{Pubkey: acctMeta.PublicKey, IsSigner: acctMeta.IsSigner, IsWritable: acctMeta.IsWritable}
		convertedAcctMetas = append(convertedAcctMetas, convertedAcctMeta)
	}

	transactionAccts := sealevel.NewTransactionAccounts(acctsForTx)
	transactionAccts.AcctMetas = convertedAcctMetas

	removeAcctsExecutableFlagChecks := slotCtx.Features.IsActive(features.RemoveAccountsExecutableFlagChecks)
	validatedLoaders := make(map[solana.PublicKey]struct{})

	for _, instr := range instrs {
		if instr.ProgramId == a.NativeLoaderAddr {
			continue
		}

		programAcct, err := slotCtx.GetAccount(instr.ProgramId)
		if err != nil {
			return nil, TxErrProgramAccountNotFound
		}

		if programAcct.Lamports == 0 {
			return nil, TxErrProgramAccountNotFound
		}

		if !removeAcctsExecutableFlagChecks && !programAcct.Executable {
			return nil, TxErrInvalidProgramForExecution
		}

		owner := programAcct.Owner
		if owner == a.NativeLoaderAddr {
			continue
		}

		_, exists := validatedLoaders[owner]
		if !exists {
			var ownerAcct *accounts.Account
			ownerAcct, err = slotCtx.GetAccount(owner)
			if err != nil {
				ownerAcct, err = slotCtx.GetAccountFromAccountsDb(owner)
				if err != nil {
					return nil, TxErrInvalidProgramForExecution
				}
			}

			if ownerAcct.Owner != a.NativeLoaderAddr || (!removeAcctsExecutableFlagChecks && !ownerAcct.Executable) {
				return nil, TxErrInvalidProgramForExecution
			}

			loadedBytesAccumulator = safemath.SaturatingAddU32(loadedBytesAccumulator, uint32(len(ownerAcct.Data)))
			if loadedBytesAccumulator > loadedAcctBytesLimit {
				return nil, TxErrMaxLoadedAccountsDataSizeExceeded
			}

			validatedLoaders[owner] = struct{}{}
		}
	}

	return transactionAccts, nil
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
	if exists {
		return true
	}

	return false
}

func (accum *loadedAcctSizeAccumulatorSimd186) add(amount uint64) error {
	accum.accumulator = safemath.SaturatingAddU64(accum.accumulator, amount)
	if accum.accumulator > accum.limit {
		return TxErrMaxLoadedAccountsDataSizeExceeded
	}
	return nil
}

func (accum *loadedAcctSizeAccumulatorSimd186) collectAcct(acct *accounts.Account) error {
	if acct.Key == sealevel.SysvarInstructionsAddr {
		return nil
	}

	acctLen := uint64(len(acct.Data))
	accum.accumulator = safemath.SaturatingAddU64(accum.accumulator, acctLen)
	if accum.accumulator > accum.limit {
		return TxErrMaxLoadedAccountsDataSizeExceeded
	}

	if acct.Owner == addresses.BpfLoaderUpgradeableAddr {
		acctState, err := sealevel.UnmarshalUpgradeableLoaderState(acct.Data)
		programDataAddr := acctState.Program.ProgramDataAddress
		if err == nil && acctState.Type == sealevel.UpgradeableLoaderStateTypeProgram {
			if !accum.wasAlreadyCounted(programDataAddr) {
				programDataAcct, err := accum.slotCtx.GetAccount(programDataAddr)
				if err != nil {
					programDataAcct, err = accum.slotCtx.GetAccountFromAccountsDb(programDataAddr)
					if err != nil {
						return TxErrInvalidProgramForExecution
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

func loadAndValidateTxAcctsSimd186(slotCtx *sealevel.SlotCtx, acctMetasPerInstr [][]sealevel.AccountMeta, tx *solana.Transaction, instrs []sealevel.Instruction, instrsAcct *accounts.Account, loadedAcctBytesLimit uint32) (*sealevel.TransactionAccounts, error) {
	acctKeys := tx.Message.AccountKeys
	accumulator := NewLoadedAcctSizeAccumulatorSimd186(slotCtx,
		uint64(loadedAcctBytesLimit),
		acctKeys)

	addrTableLookupCost := safemath.SaturatingMulU64(uint64(len(tx.Message.AddressTableLookups)), addrLookupTableBaseSize)
	err := accumulator.add(addrTableLookupCost)
	if err != nil {
		return nil, err
	}

	for _, pubkey := range acctKeys[1:] {
		acct, err := slotCtx.GetAccount(pubkey)
		if err != nil {
			panic("should be impossible - programming error")
		}
		err = accumulator.collectAcct(acct)
		if err != nil {
			return nil, err
		}
	}

	txAcctMetas, err := tx.AccountMetaList()
	if err != nil {
		return nil, err
	}

	var programIdIdxs []uint64
	instructionAcctPubkeys := make(map[solana.PublicKey]struct{})

	for instrIdx, instr := range tx.Message.Instructions {
		programIdIdxs = append(programIdIdxs, uint64(instr.ProgramIDIndex))
		ias := acctMetasPerInstr[instrIdx]
		for _, ia := range ias {
			instructionAcctPubkeys[ia.Pubkey] = struct{}{}
		}
	}

	acctsForTx := make([]accounts.Account, 0, len(txAcctMetas))
	convertedAcctMetas := make([]*sealevel.AccountMeta, 0, len(txAcctMetas))

	for idx, acctMeta := range txAcctMetas {
		var acct *accounts.Account

		_, instrContainsAcctMeta := instructionAcctPubkeys[acctMeta.PublicKey]
		if acctMeta.PublicKey == sealevel.SysvarInstructionsAddr {
			acct = instrsAcct
		} else if !slotCtx.Features.IsActive(features.DisableAccountLoaderSpecialCase) && slices.Contains(programIdIdxs, uint64(idx)) && !acctMeta.IsWritable && !instrContainsAcctMeta {
			tmp, err := slotCtx.GetAccount(acctMeta.PublicKey)
			if err != nil {
				return nil, err
			}
			acct = &accounts.Account{Key: acctMeta.PublicKey, Owner: tmp.Owner, Executable: true, IsDummy: true}
		} else {
			acct, err = slotCtx.GetAccount(acctMeta.PublicKey)
			if err != nil {
				return nil, err
			}
		}

		acctsForTx = append(acctsForTx, *acct)
		convertedAcctMeta := &sealevel.AccountMeta{Pubkey: acctMeta.PublicKey, IsSigner: acctMeta.IsSigner, IsWritable: acctMeta.IsWritable}
		convertedAcctMetas = append(convertedAcctMetas, convertedAcctMeta)
	}

	transactionAccts := sealevel.NewTransactionAccounts(acctsForTx)
	transactionAccts.AcctMetas = convertedAcctMetas

	removeAcctsExecutableFlagChecks := slotCtx.Features.IsActive(features.RemoveAccountsExecutableFlagChecks)

	for _, instr := range instrs {
		if instr.ProgramId == a.NativeLoaderAddr {
			continue
		}

		programAcct, err := slotCtx.GetAccount(instr.ProgramId)
		if err != nil {
			programAcct, err = slotCtx.GetAccountFromAccountsDb(instr.ProgramId)
			return nil, TxErrProgramAccountNotFound
		}

		if programAcct.Lamports == 0 {
			return nil, TxErrProgramAccountNotFound
		}

		if !removeAcctsExecutableFlagChecks && !programAcct.Executable {
			return nil, TxErrInvalidProgramForExecution
		}

		owner := programAcct.Owner
		if owner != a.NativeLoaderAddr && !isLoaderAcct(owner) {
			return nil, TxErrInvalidProgramForExecution
		}
	}

	return transactionAccts, nil
}

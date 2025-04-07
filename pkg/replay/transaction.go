package replay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/fees"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/rent"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/util"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type TxErrInvalidSignature struct {
	msg string
}

func NewTxErrInvalidSignature(msg string) error {
	return &TxErrInvalidSignature{msg: msg}
}

func (err *TxErrInvalidSignature) Error() string {
	return err.msg
}

var (
	TxErrInsufficientFundsForRent = errors.New("TxErrInsufficientFundsForRent")
)

func transactionAcctsFromTx(slotCtx *sealevel.SlotCtx, tx *solana.Transaction) (*sealevel.TransactionAccounts, error) {
	txAcctMetas, err := tx.AccountMetaList()
	if err != nil {
		return nil, err
	}

	var programIdIdxs []uint64
	var instructionAcctPubkeys []solana.PublicKey

	for _, instr := range tx.Message.Instructions {
		programIdIdxs = append(programIdIdxs, uint64(instr.ProgramIDIndex))
		ias, err := instr.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			panic("unable to resolve instruction accts")
		}
		for _, ia := range ias {
			instructionAcctPubkeys = append(instructionAcctPubkeys, ia.PublicKey)
		}
	}
	instructionAcctPubkeys = util.DedupePubkeys(instructionAcctPubkeys)

	acctsForTx := make([]accounts.Account, 0, len(txAcctMetas))
	for idx, acctMeta := range txAcctMetas {
		var acct *accounts.Account

		if slices.Contains(programIdIdxs, uint64(idx)) && !acctMeta.IsWritable && !slices.Contains(instructionAcctPubkeys, acctMeta.PublicKey) {
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
	}

	transactionAccts := sealevel.NewTransactionAccounts(acctsForTx)
	return transactionAccts, nil
}

func programIndices(tx *solana.Transaction, instrIdx int) []uint64 {
	idx := uint64(tx.Message.Instructions[instrIdx].ProgramIDIndex)
	return []uint64{idx}
}

func newExecCtx(slotCtx *sealevel.SlotCtx, transactionAccts *sealevel.TransactionAccounts, computeBudgetLimits *sealevel.ComputeBudgetLimits, log *sealevel.LogRecorder) *sealevel.ExecutionCtx {
	txCtx := sealevel.NewTransactionCtx(*transactionAccts, 64, 64)
	execCtx := &sealevel.ExecutionCtx{Log: log, TransactionContext: txCtx, ComputeMeter: cu.NewComputeMeter(uint64(computeBudgetLimits.ComputeUnitLimit))}

	execCtx.GlobalCtx.Features = *slotCtx.Features
	execCtx.Accounts = accounts.NewMemAccounts()
	execCtx.SlotCtx = slotCtx
	execCtx.TransactionContext.ComputeBudgetLimits = computeBudgetLimits
	//execCtx.ComputeMeter.Disable()

	return execCtx
}

func instrsFromTx(tx *solana.Transaction, f *features.Features) ([]sealevel.Instruction, error) {
	instrs := make([]sealevel.Instruction, len(tx.Message.Instructions))
	for idx, compiledInstr := range tx.Message.Instructions {
		programId, err := tx.ResolveProgramIDIndex(compiledInstr.ProgramIDIndex)
		if err != nil {
			return nil, err
		}

		ams, err := compiledInstr.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			return nil, err
		}

		var acctMetas []sealevel.AccountMeta
		for _, am := range ams {
			acctMeta := sealevel.AccountMeta{Pubkey: am.PublicKey, IsSigner: am.IsSigner, IsWritable: isWritable(tx, am, f)}
			acctMetas = append(acctMetas, acctMeta)
		}

		instr := sealevel.Instruction{Accounts: acctMetas, ProgramId: programId, Data: compiledInstr.Data}
		instrs[idx] = instr
	}

	return instrs, nil
}

func fixupInstructionsSysvarAcct(execCtx *sealevel.ExecutionCtx, instrIdx uint16) error {
	instructionsSysvarIdx, err := execCtx.TransactionContext.IndexOfAccount(sealevel.SysvarInstructionsAddr)
	if err == nil {
		instructionsAcct, err := execCtx.TransactionContext.AccountAtIndex(instructionsSysvarIdx)
		if err != nil {
			return err
		}

		lastIndex := len(instructionsAcct.Data) - 2
		binary.LittleEndian.PutUint16(instructionsAcct.Data[lastIndex:], instrIdx)
		mlog.Log.Debugf("found instructions sysvar pubkey at instr idx %d", instrIdx)
	}
	return nil
}

var newReservedAccts = []solana.PublicKey{sealevel.AddressLookupTableAddr, sealevel.ComputeBudgetProgramAddr,
	sealevel.Ed25519PrecompileAddr, sealevel.LoaderV4Addr, sealevel.Secp256kPrecompileAddr, sealevel.ZkElgamalProofProgramAddr,
	sealevel.ZkTokenProofProgramAddr, sealevel.SysvarEpochRewardsAddr, sealevel.SysvarLastRestartSlotAddr, sealevel.SysvarOwnerAddr}

func isWritable(tx *solana.Transaction, am *solana.AccountMeta, f *features.Features) bool {
	if !am.IsWritable {
		return false
	}

	if isNativeProgram(am.PublicKey) || isSysvar(am.PublicKey) {
		return false
	}

	if f.IsActive(features.AddNewReservedAccountKeys) {
		if slices.Contains(newReservedAccts, am.PublicKey) {
			return false
		}
	}

	if f.IsActive(features.EnableSecp256r1Precompile) {
		if am.PublicKey == sealevel.Secp256r1PrecompileAddr {
			return false
		}
	}

	programIds, err := tx.GetProgramIDs()
	if err != nil {
		panic(err)
	}

	for _, programId := range programIds {
		if am.PublicKey == programId {
			return false
		}
	}

	return true
}

func recordModifiedAccounts(slotCtx *sealevel.SlotCtx, execCtx *sealevel.ExecutionCtx) {
	// update account states in slotCtx for all accounts 'touched' during the tx's execution
	for idx, newAcctState := range execCtx.TransactionContext.Accounts.Accounts {
		if execCtx.TransactionContext.Accounts.Touched[idx] {
			err := slotCtx.SetAccount(newAcctState.Key, newAcctState)
			if err != nil {
				panic(fmt.Sprintf("unable to set slot account for %s to update state: %s", newAcctState.Key, err))
			}
			slotCtx.ModifiedAccts[newAcctState.Key] = true
			mlog.Log.Debugf("modified account %s after tx", newAcctState.Key)
		}
	}
}

func recordStakeDelegation(slotCtx *sealevel.SlotCtx, acct *accounts.Account) {
	isEmpty := acct.Lamports == 0
	isUninitialized := true

	if len(acct.Data) >= 4 {
		acctType := binary.LittleEndian.Uint32(acct.Data)
		isUninitialized = acctType == sealevel.StakeStateV2StatusUninitialized
	}

	if isEmpty || isUninitialized {
		delete(slotCtx.StakeAccts, acct.Key)
	} else {
		mlog.Log.Debugf("added stake delegation record for %s: %v", acct.Key, acct)
		slotCtx.StakeAccts[acct.Key] = true
	}
}

func recordVoteTimestampAndSlot(slotCtx *sealevel.SlotCtx, acct *accounts.Account) {
	voteStateVersioned := new(sealevel.VoteStateVersions)
	decoder := bin.NewBinDecoder(acct.Data)

	err := voteStateVersioned.UnmarshalWithDecoder(decoder)
	if err != nil {
		panic(fmt.Sprintf("unable to deserialize versioned vote state - shouldn't be possible. %s", err))
	}

	var timestamp sealevel.BlockTimestamp

	switch voteStateVersioned.Type {
	case sealevel.VoteStateVersionCurrent:
		timestamp = voteStateVersioned.Current.LastTimestamp

	case sealevel.VoteStateVersionV0_23_5:
		timestamp = voteStateVersioned.V0_23_5.LastTimestamp

	case sealevel.VoteStateVersionV1_14_11:
		timestamp = voteStateVersioned.V1_14_11.LastTimestamp
	}

	slotCtx.VoteTimestamps[acct.Key] = timestamp
}

func recordStakeAndVoteAccounts(slotCtx *sealevel.SlotCtx, execCtx *sealevel.ExecutionCtx, writablePubkeys []solana.PublicKey) {
	modifiedVoteAccts := execCtx.TransactionContext.ModifiedVoteAccts
	modifiedStakeAccts := execCtx.TransactionContext.ModifiedStakeAccts

	for _, acct := range execCtx.TransactionContext.Accounts.Accounts {
		if !slices.Contains(writablePubkeys, acct.Key) {
			continue
		}

		if modifiedVoteAccts && acct.Owner == sealevel.VoteProgramAddr {
			recordVoteTimestampAndSlot(slotCtx, acct)
		}

		if modifiedStakeAccts && acct.Owner == sealevel.StakeProgramAddr {
			recordStakeDelegation(slotCtx, acct)
		}
	}
}

func handleDurableNonceIfEligibleFailedTx(instrs []sealevel.Instruction, tx *solana.Transaction, execCtx *sealevel.ExecutionCtx, slotCtx *sealevel.SlotCtx) (solana.PublicKey, bool) {
	if !execCtx.TransactionContext.NonceAcctAdvanced {
		return solana.PublicKey{}, false
	}

	recentBlockhashes, err := sealevel.ReadRecentBlockHashesSysvar(execCtx)
	if err != nil {
		panic(fmt.Sprintf("unable to decode recentblockhashes sysvar: %s", err))
	}

	if recentBlockhashes.IsBlockhashAgeValid(tx.Message.RecentBlockhash) {
		return solana.PublicKey{}, false
	}

	instr := instrs[0]

	if instr.ProgramId == sealevel.SystemProgramAddr && len(instr.Data) >= 4 {
		decoder := bin.NewBinDecoder(instr.Data)

		instructionType, err := decoder.ReadUint32(bin.LE)
		if err != nil {
			return solana.PublicKey{}, false
		}

		if instructionType == sealevel.SystemProgramInstrTypeAdvanceNonceAccount {
			nonceAcctPk := instr.Accounts[0].Pubkey
			var nonceAcct *accounts.Account
			for _, acct := range execCtx.TransactionContext.Accounts.Accounts {
				if acct.Key == nonceAcctPk {
					nonceAcct = acct
					break
				}
			}

			if nonceAcct == nil {
				panic("nonce account not found in transaction accounts")
			}

			err = slotCtx.SetAccount(nonceAcctPk, nonceAcct)
			if err != nil {
				panic(fmt.Sprintf("error setting nonce account state after failed tx: %s\n", err))
			}

			return instr.Accounts[0].Pubkey, true
		}
	}

	return solana.PublicKey{}, false
}

func ProcessTransaction(slotCtx *sealevel.SlotCtx, tx *solana.Transaction, txMeta *rpc.TransactionMeta) (*fees.TxFeeInfo, []solana.PublicKey, error) {
	/*err := tx.VerifySignatures()
	if err != nil {
		return NewTxErrInvalidSignature(err.Error())
	}*/

	instrs, err := instrsFromTx(tx, slotCtx.Features)
	if err != nil {
		return nil, nil, err
	}

	err = sealevel.WriteInstructionsSysvar(&slotCtx.Accounts, instrs)
	if err != nil {
		return nil, nil, err
	}

	transactionAccts, err := transactionAcctsFromTx(slotCtx, tx)
	if err != nil {
		return nil, nil, err
	}

	computeBudgetLimits, err := sealevel.ComputeBudgetExecuteInstructions(instrs)
	if err != nil {
		return nil, nil, err
	}

	var log sealevel.LogRecorder
	execCtx := newExecCtx(slotCtx, transactionAccts, computeBudgetLimits, &log)
	execCtx.TransactionContext.AllInstructions = instrs
	execCtx.TransactionContext.Signature = tx.Signatures[0]

	// check for pre-balance divergences
	for count := uint64(0); count < uint64(len(tx.Message.AccountKeys)); count++ {
		txAcct, err := execCtx.TransactionContext.Accounts.GetAccount(count)
		if err != nil {
			panic(fmt.Sprintf("unable to get tx acct %d whilst checking for pre-balances divergences", count))
		}

		if !isNativeProgram(txAcct.Key) && !txAcct.IsDummy {
			if txAcct.Lamports != txMeta.PreBalances[count] {
				mlog.Log.Infof("tx %s pre-balance divergence: lamport balance for %s was %d but onchain lamport balance was %d\n%s", tx.Signatures[0], txAcct.Key, txAcct.Lamports, txMeta.PreBalances[count], util.PrettyPrintAcct(txAcct))
			}
		}

		execCtx.TransactionContext.Accounts.Unlock(count)
	}

	txFeeInfo, payerNewLamports, err := fees.CalculateAndDeductTxFees(tx, instrs, &execCtx.TransactionContext.Accounts, computeBudgetLimits)
	if err != nil {
		return txFeeInfo, nil, nil
	}

	// check for fee divergences
	if txFeeInfo.TotalFee != txMeta.Fee {
		mlog.Log.Infof("tx %s fee divergence: totalFee was %d, but onchain fee was %d", tx.Signatures[0], txFeeInfo.TotalFee, txMeta.Fee)
	}

	rentSysvar, err := sealevel.ReadRentSysvar(execCtx)
	if err != nil {
		panic("failed to get and deserialize rent sysvar")
	}

	rent.MaybeSetRentExemptRentEpochMax(slotCtx, &rentSysvar, &execCtx.GlobalCtx.Features, &execCtx.TransactionContext.Accounts)
	preTxRentStates := rent.NewRentStateInfo(&rentSysvar, execCtx.TransactionContext, tx)

	var instrErr error
	writablePubkeys := make([]solana.PublicKey, 0, 64)

	foundAcct := false
	if tx.Signatures[0] == solana.MustSignatureFromBase58("5PhE791wXTUf5TEhDUGFJV3tTfUwjYbjQLBBS57RPq3472dh1LFzhwNphHxZUjqk2dcaqZAY9mbPPTxEuFkPuPYJ") {
		for _, txAcct := range execCtx.TransactionContext.Accounts.Accounts {
			mlog.Log.Infof("pre-tx acct: %s", util.PrettyPrintAcct(txAcct))
			if txAcct.Key == solana.MustPublicKeyFromBase58("SysvarS1otHistory11111111111111111111111111") && foundAcct {
				mlog.Log.Infof("slothistory data: %d", txAcct.Data)
			}
		}
	}

	for instrIdx, instr := range tx.Message.Instructions {
		err = fixupInstructionsSysvarAcct(execCtx, uint16(instrIdx))
		if err != nil {
			return txFeeInfo, nil, err
		}

		resolvedAccountMetas, err := instr.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			return txFeeInfo, nil, err
		}

		var acctMetas []sealevel.AccountMeta
		for _, am := range resolvedAccountMetas {
			acctMeta := sealevel.AccountMeta{Pubkey: am.PublicKey, IsSigner: am.IsSigner, IsWritable: isWritable(tx, am, &execCtx.GlobalCtx.Features)}
			acctMetas = append(acctMetas, acctMeta)
			if tx.Signatures[0] == solana.MustSignatureFromBase58("5PhE791wXTUf5TEhDUGFJV3tTfUwjYbjQLBBS57RPq3472dh1LFzhwNphHxZUjqk2dcaqZAY9mbPPTxEuFkPuPYJ") {
				mlog.Log.Infof("instr acct: %+v\n", acctMeta)
			}
		}

		instructionAccts := sealevel.InstructionAcctsFromAccountMetas(acctMetas, *transactionAccts)

		err = execCtx.ProcessInstruction(instr.Data, instructionAccts, programIndices(tx, instrIdx))
		if err == nil {
			for _, am := range acctMetas {
				if am.IsWritable {
					writablePubkeys = append(writablePubkeys, am.Pubkey)
				}
			}
		} else {
			mlog.Log.Debugf("%+v", tx)
			instrErr = err
			break
		}
	}

	txAcctMetas, err := tx.AccountMetaList()
	if err != nil {
		panic(err)
	}

	for _, txAcctMeta := range txAcctMetas {
		if isWritable(tx, txAcctMeta, &execCtx.GlobalCtx.Features) {
			writablePubkeys = append(writablePubkeys, txAcctMeta.PublicKey)
		}
	}

	mlog.Log.Debugf("[+] tx %s - compute units consumed: %d", tx.Signatures[0], execCtx.ComputeMeter.Used())

	if len(log.Logs) != 0 && tx.Signatures[0] == solana.MustSignatureFromBase58("5PhE791wXTUf5TEhDUGFJV3tTfUwjYbjQLBBS57RPq3472dh1LFzhwNphHxZUjqk2dcaqZAY9mbPPTxEuFkPuPYJ") {
		mlog.Log.Infof("\ntx logs:\n")
		for _, logEntry := range log.Logs {
			mlog.Log.Infof("%s\n", logEntry)
		}
	}

	// check for CU consumed divergences
	if instrErr == nil && *txMeta.ComputeUnitsConsumed != execCtx.ComputeMeter.Used() {
		mlog.Log.Debugf("tx %s CU divergence: used was %d but onchain CU consumed was %d (%d discrepancy)", tx.Signatures[0], execCtx.ComputeMeter.Used(), *txMeta.ComputeUnitsConsumed, max(execCtx.ComputeMeter.Used(), *txMeta.ComputeUnitsConsumed)-min(execCtx.ComputeMeter.Used(), *txMeta.ComputeUnitsConsumed))
	}

	postTxRentStates := rent.NewRentStateInfo(&rentSysvar, execCtx.TransactionContext, tx)
	rentStateErr := rent.VerifyRentStateChanges(preTxRentStates, postTxRentStates, execCtx.TransactionContext)

	if tx.Signatures[0] == solana.MustSignatureFromBase58("5PhE791wXTUf5TEhDUGFJV3tTfUwjYbjQLBBS57RPq3472dh1LFzhwNphHxZUjqk2dcaqZAY9mbPPTxEuFkPuPYJ") {
		for _, txAcct := range execCtx.TransactionContext.Accounts.Accounts {
			mlog.Log.Infof("post-tx acct: %s", util.PrettyPrintAcct(txAcct))
			if txAcct.Key == solana.MustPublicKeyFromBase58("SysvarS1otHistory11111111111111111111111111") && foundAcct {
				mlog.Log.Infof("slothistory data: %d", txAcct.Data)
			}
		}
	}

	// check for post-balances divergences (but only if the tx succeeded)
	if instrErr == nil && rentStateErr == nil {
		for count := uint64(0); count < uint64(len(tx.Message.AccountKeys)); count++ {
			txAcct, err := execCtx.TransactionContext.Accounts.GetAccount(count)
			if err != nil {
				panic(fmt.Sprintf("unable to get tx acct %d whilst checking for post-balances divergences", count))
			}

			if !isNativeProgram(txAcct.Key) && !txAcct.IsDummy {
				if txAcct.Lamports != txMeta.PostBalances[count] {
					mlog.Log.Infof("tx %s post-balance divergence: lamport balance for %s was %d but onchain lamport balance was %d\n%s\n", tx.Signatures[0], txAcct.Key, txAcct.Lamports, txMeta.PostBalances[count], util.PrettyPrintAcct(txAcct))
				}
			}
			execCtx.TransactionContext.Accounts.Unlock(count)
		}
	}

	payerAcct, err := execCtx.TransactionContext.Accounts.GetAccount(0)
	if err != nil {
		panic(fmt.Sprintf("unable to get tx account to update payer acct state after failed tx: %s", err))
	}

	// if there was an error in the tx, do not update account states, except for deducting the tx fee
	// from the payer account
	if instrErr != nil || rentStateErr != nil {
		p, err := slotCtx.GetAccount(payerAcct.Key)
		if err != nil {
			panic(fmt.Sprintf("unable to get slot account to update payer acct state after failed tx: %s", err))
		}

		p.Lamports = payerNewLamports
		err = slotCtx.SetAccount(payerAcct.Key, p)
		if err != nil {
			panic(fmt.Sprintf("unable to set slot account to update state of payer acct after failed t: %s", err))
		}

		slotCtx.ModifiedAccts[payerAcct.Key] = true
		execCtx.TransactionContext.Accounts.Unlock(0)

		writableAcctsForFailedTx := make([]solana.PublicKey, 0)
		writableAcctsForFailedTx = append(writableAcctsForFailedTx, payerAcct.Key)

		noncePubkey, isEligibleDurableTx := handleDurableNonceIfEligibleFailedTx(instrs, tx, execCtx, slotCtx)
		if isEligibleDurableTx {
			writableAcctsForFailedTx = append(writableAcctsForFailedTx, noncePubkey)
		}

		var txErr error
		if rentStateErr != nil {
			txErr = rentStateErr
		} else {
			txErr = instrErr
		}

		return txFeeInfo, writableAcctsForFailedTx, fmt.Errorf("tx err: %s", txErr)
	}

	recordModifiedAccounts(slotCtx, execCtx)
	writablePubkeys = append(writablePubkeys, payerAcct.Key)
	recordStakeAndVoteAccounts(slotCtx, execCtx, writablePubkeys)

	return txFeeInfo, writablePubkeys, nil
}

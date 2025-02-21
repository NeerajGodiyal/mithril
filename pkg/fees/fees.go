package fees

import (
	"fmt"
	"math"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/safemath"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/wide"
	"github.com/gagliardetto/solana-go"
	"k8s.io/klog/v2"
)

const microLamportsPerLamport = 1000000

func calculatePriorityFee(computeBudgetLimits *sealevel.ComputeBudgetLimits) (uint64, error) {
	computeUnitPrice := wide.Uint128FromUint64(computeBudgetLimits.ComputeUnitPrice)
	computeUnitLimit := wide.Uint128FromUint64(uint64(computeBudgetLimits.ComputeUnitLimit))

	microLamportFee, err := safemath.CheckedMulU128(computeUnitPrice, computeUnitLimit)
	if err != nil {
		return 0, err
	}

	fee := safemath.SaturatingAddU128(microLamportFee, wide.Uint128FromUint64(microLamportsPerLamport-1)).Div(wide.Uint128FromUint64(microLamportsPerLamport))

	var priorityFee uint64
	if fee.IsUint64() {
		priorityFee = fee.Uint64()
	} else {
		priorityFee = math.MaxUint64
	}

	return priorityFee, nil
}

// There are currently two aspects of the tx fee cost model on Solana
// 1) fee per signature (5k lamports/sig)
// 2) prioritization fees set via a SetComputeUnitPrice instruction

const feePayerIdx = 0

type TxFeeInfo struct {
	ExecutionFee uint64
	PriorityFee  uint64
	TotalFee     uint64
}

type TxFeeInfoAccumulator struct {
	ExecutionFees uint64
	PriorityFees  uint64
	TotalFees     uint64
}

func (txFeeAccumulator *TxFeeInfoAccumulator) Add(txFeeInfo *TxFeeInfo) {
	var err error

	txFeeAccumulator.TotalFees, err = safemath.CheckedAddU64(txFeeAccumulator.TotalFees, txFeeInfo.TotalFee)
	if err != nil {
		panic("overflow in accumulating total tx fees - should be impossible")
	}

	txFeeAccumulator.PriorityFees, err = safemath.CheckedAddU64(txFeeAccumulator.PriorityFees, txFeeInfo.PriorityFee)
	if err != nil {
		panic("overflow in accumulating priority fees - should be impossible")
	}

	txFeeAccumulator.ExecutionFees, err = safemath.CheckedAddU64(txFeeAccumulator.ExecutionFees, txFeeInfo.ExecutionFee)
	if err != nil {
		panic("overflow in accumulating execution fees - should be impossible")
	}
}

func CalculateAndDeductTxFees(tx *solana.Transaction, instrs []sealevel.Instruction, transactionAccts *sealevel.TransactionAccounts, computeBudgetLimits *sealevel.ComputeBudgetLimits) (*TxFeeInfo, uint64, error) {
	feePayerAcct, err := transactionAccts.GetAccount(feePayerIdx)
	if err != nil {
		panic("no fee payer")
	}
	defer transactionAccts.Unlock(feePayerIdx)

	numSignatures := uint64(tx.Message.Header.NumRequiredSignatures)

	// have to pay fees per signatures to these precompiles as well
	for _, instr := range instrs {
		if instr.ProgramId == sealevel.Secp256kPrecompileAddr || instr.ProgramId == sealevel.Ed25519PrecompileAddr {
			if len(instr.Data) == 0 {
				continue
			} else {
				numSignatures += uint64(instr.Data[0])
			}
		}
	}

	// basic tx fee. 5000 lamports per signature.
	baseTxFee := numSignatures * 5000

	// prioritization fees
	var priorityFee uint64
	if computeBudgetLimits.ComputeUnitPrice != 0 {
		priorityFee, err = calculatePriorityFee(computeBudgetLimits)
		if err != nil {
			panic("overflow in calculating priority fee - shouldn't be possible")
		}
	}

	totalTxFee, err := safemath.CheckedAddU64(baseTxFee, priorityFee)
	if err != nil {
		panic("overflow in calculating total tx fee")
	}

	feeInfo := &TxFeeInfo{ExecutionFee: baseTxFee, PriorityFee: priorityFee, TotalFee: totalTxFee}

	if feePayerAcct.Lamports < totalTxFee {
		return feeInfo, 0, sealevel.InstrErrInsufficientFunds
	}

	klog.Infof("tx fee: %d", totalTxFee)

	feePayerAcct.Lamports -= totalTxFee
	transactionAccts.Touch(feePayerIdx)

	return feeInfo, feePayerAcct.Lamports, nil
}

func DistributeTxFeesToSlotLeader(acctsDb *accountsdb.AccountsDb, slotCtx *sealevel.SlotCtx, leader solana.PublicKey, txFeeAccumulator *TxFeeInfoAccumulator) {
	var feesToBurn uint64
	var feesToLeader uint64

	if slotCtx.Features.IsActive(features.RewardFullPriorityFee) {
		halfFee := txFeeAccumulator.ExecutionFees / 2
		feesToLeader = safemath.SaturatingAddU64(txFeeAccumulator.PriorityFees, txFeeAccumulator.ExecutionFees-halfFee)
		feesToBurn = halfFee
	} else {
		feesToBurn = txFeeAccumulator.TotalFees / 2
		feesToLeader = txFeeAccumulator.TotalFees - feesToBurn
	}

	var leaderAcct *accounts.Account
	var err error

	leaderAcct, err = slotCtx.GetAccount(leader)
	if err != nil {
		// if leader didn't appear at all in the block, then retrieve its latest state from accountsdb instead
		leaderAcct, err = acctsDb.GetAccount(slotCtx.Slot, leader)
		if err != nil {
			panic(fmt.Sprintf("unable to get leader acct %s from both slotCtx and accountsdb", leader))
		}
	}

	leaderAcct.Lamports, err = safemath.CheckedAddU64(leaderAcct.Lamports, feesToLeader)
	if err != nil {
		panic("overflow when adding reward to slot leader balance")
	}

	err = slotCtx.SetAccount(leader, leaderAcct)
	if err != nil {
		panic(fmt.Sprintf("failed to SetAccount for leader acct %s when distributing tx fees", leader))
	}

	klog.Infof("calculated fees for leader: %d, post-balance: %d (%s)", feesToLeader, leaderAcct.Lamports, leader)
}

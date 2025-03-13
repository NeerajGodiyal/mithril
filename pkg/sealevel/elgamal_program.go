package sealevel

import (
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

const (
	ProofInstrCloseContextState = iota
	ProofInstrVerifyZeroCiphertext
	ProofInstrVerifyCiphertextCiphertextEquality
	ProofInstrVerifyCiphertextCommitmentEquality
	ProofInstrVerifyPubkeyValidity
	ProofInstrVerifyPercentageWithCap
	ProofInstrVerifyBatchedRangeProofU64
	ProofInstrVerifyBatchedRangeProofU128
	ProofInstrVerifyBatchedRangeProofU256
	ProofInstrVerifyGroupedCiphertext2HandlesValidity
	ProofInstrVerifyBatchedGroupedCiphertext2HandlesValidity
	ProofInstrVerifyGroupedCiphertext3HandlesValidity
	ProofInstrVerifyBatchedGroupedCiphertext3HandlesValidity
)

type ElGamalProofContextStateMeta struct {
	ContextStateAuthority solana.PublicKey
	ProofType             byte
}

func (proofCtxStateMeta *ElGamalProofContextStateMeta) UnmarshalWithDecoder(decoder *bin.Decoder) error {
	authorityPk, err := decoder.ReadBytes(32)
	if err != nil {
		return err
	}
	copy(proofCtxStateMeta.ContextStateAuthority[:], authorityPk)

	proofCtxStateMeta.ProofType, err = decoder.ReadByte()
	return err
}

func ElGamalExecute(execCtx *ExecutionCtx) error {
	txCtx := execCtx.TransactionContext

	instrCtx, err := txCtx.CurrentInstructionCtx()
	if err != nil {
		return err
	}

	if len(instrCtx.Data) < 1 {
		return InstrErrInvalidInstructionData
	}

	instrType := instrCtx.Data[0]

	switch instrType {
	case ProofInstrCloseContextState:
		{
			err = execCtx.ComputeMeter.Consume(CUCloseContextStateComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("CloseContextState")

			err = processCloseProofContext(execCtx)
		}

	case ProofInstrVerifyZeroCiphertext:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyZeroCiphertextComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyZeroCiphertext")

			// process_verify_proof
		}

	case ProofInstrVerifyCiphertextCiphertextEquality:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyCiphertextCiphertextEqualityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyCiphertextCiphertextEquality")

			// process_verify_proof
		}

	case ProofInstrVerifyCiphertextCommitmentEquality:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyCiphertextCommitmentEqualityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyCiphertextCommitmentEquality")

			// process_verify_proof
		}

	case ProofInstrVerifyPubkeyValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyPubkeyValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyPubkeyValidity")

			// process_verify_proof
		}

	case ProofInstrVerifyPercentageWithCap:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyPercentageWithCapComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyPercentageWithCap")

			// process_verify_proof
		}

	case ProofInstrVerifyBatchedRangeProofU64:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU64ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU64")

			// process_verify_proof
		}

	case ProofInstrVerifyBatchedRangeProofU128:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU128ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU128")

			// process_verify_proof
		}

	case ProofInstrVerifyBatchedRangeProofU256:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU256ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU256")

			// process_verify_proof
		}

	case ProofInstrVerifyGroupedCiphertext2HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyGroupedCiphertext2HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyGroupedCiphertext2HandlesValidity")

			// process_verify_proof
		}

	case ProofInstrVerifyBatchedGroupedCiphertext2HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedGroupedCiphertext2HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedGroupedCiphertext2HandlesValidity")

			// process_verify_proof
		}

	case ProofInstrVerifyGroupedCiphertext3HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyGroupedCiphertext3HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyGroupedCiphertext3HandlesValidity")

			// process_verify_proof
		}

	case ProofInstrVerifyBatchedGroupedCiphertext3HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedGroupedCiphertext3HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedGroupedCiphertext3HandlesValidity")

			// process_verify_proof
		}
	}

	return err
}

func processCloseProofContext(execCtx *ExecutionCtx) error {
	txCtx := execCtx.TransactionContext

	instrCtx, err := txCtx.CurrentInstructionCtx()
	if err != nil {
		return err
	}

	ownerAcct, err := instrCtx.BorrowInstructionAccount(txCtx, 2)
	if err != nil {
		return err
	}

	if !ownerAcct.IsSigner() {
		return InstrErrMissingRequiredSignature
	}

	ownerPubkey := ownerAcct.Key()
	ownerAcct.Drop()

	proofContextAcct, err := instrCtx.BorrowInstructionAccount(txCtx, 0)
	if err != nil {
		return err
	}
	proofContextAcctPubkey := proofContextAcct.Key()
	proofContextAcct.Drop()

	destinationAcct, err := instrCtx.BorrowInstructionAccount(txCtx, 1)
	if err != nil {
		return err
	}
	destinationAcctPubkey := destinationAcct.Key()
	destinationAcct.Drop()

	if proofContextAcctPubkey == destinationAcctPubkey {
		return InstrErrInvalidInstructionData
	}

	proofContextAcct, err = instrCtx.BorrowInstructionAccount(txCtx, 0)
	if err != nil {
		return err
	}

	decoder := bin.NewBinDecoder(proofContextAcct.Data())
	var proofContextStateMeta ElGamalProofContextStateMeta
	err = proofContextStateMeta.UnmarshalWithDecoder(decoder)
	if err != nil {
		return InstrErrInvalidAccountData
	}

	expectedOwnerPubkey := proofContextStateMeta.ContextStateAuthority

	if ownerPubkey != expectedOwnerPubkey {
		return InstrErrInvalidAccountOwner
	}

	destinationAcct, err = instrCtx.BorrowInstructionAccount(txCtx, 1)
	if err != nil {
		return err
	}

	err = destinationAcct.CheckedAddLamports(proofContextAcct.Lamports(), execCtx.GlobalCtx.Features)
	if err != nil {
		return err
	}

	err = proofContextAcct.SetLamports(0, execCtx.GlobalCtx.Features)
	if err != nil {
		return err
	}

	err = proofContextAcct.SetDataLength(0, execCtx.GlobalCtx.Features)
	if err != nil {
		return err
	}

	err = proofContextAcct.SetOwner(execCtx.GlobalCtx.Features, SystemProgramAddr)

	proofContextAcct.Drop()
	destinationAcct.Drop()

	return err
}

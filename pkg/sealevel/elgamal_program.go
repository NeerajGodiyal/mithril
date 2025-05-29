package sealevel

import (
	"encoding/binary"

	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/zksdk"
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

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyCiphertextCiphertextEquality:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyCiphertextCiphertextEqualityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyCiphertextCiphertextEquality")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyCiphertextCommitmentEquality:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyCiphertextCommitmentEqualityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyCiphertextCommitmentEquality")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyPubkeyValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyPubkeyValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyPubkeyValidity")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyPercentageWithCap:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyPercentageWithCapComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyPercentageWithCap")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyBatchedRangeProofU64:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU64ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU64")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyBatchedRangeProofU128:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU128ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU128")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyBatchedRangeProofU256:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedRangeProofU256ComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedRangeProofU256")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyGroupedCiphertext2HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyGroupedCiphertext2HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyGroupedCiphertext2HandlesValidity")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyBatchedGroupedCiphertext2HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedGroupedCiphertext2HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedGroupedCiphertext2HandlesValidity")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyGroupedCiphertext3HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyGroupedCiphertext3HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyGroupedCiphertext3HandlesValidity")

			err = processVerifyProof(execCtx)
		}

	case ProofInstrVerifyBatchedGroupedCiphertext3HandlesValidity:
		{
			err = execCtx.ComputeMeter.Consume(CUVerifyBatchedGroupedCiphertext3HandlesValidityComputeUnits)
			if err != nil {
				return InstrErrComputationalBudgetExceeded
			}

			mlog.Log.Infof("VerifyBatchedGroupedCiphertext3HandlesValidity")

			err = processVerifyProof(execCtx)
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

	err = proofContextAcct.SetOwner(execCtx.GlobalCtx.Features, a.SystemProgramAddr)

	proofContextAcct.Drop()
	destinationAcct.Drop()

	return err
}

const (
	instrDataLenWithProofAcct = 5
	ctxHdrLen                 = 33
)

var ctxObjLens []uint64 = []uint64{
	0,
	96,
	128,
	128,
	32,
	104,
	264,
	264,
	264,
	160,
	256,
	224,
	352}

var proofLens []uint64 = []uint64{
	0,
	96,
	224,
	192,
	64,
	256,
	672,
	736,
	800,
	160,
	160,
	192,
	192,
}

type ZkVerificationFn func([]byte) error

func getZkVerificationFn(instrType byte) ZkVerificationFn {
	switch instrType {
	case ProofInstrVerifyZeroCiphertext:
		{
			return zksdk.VerifyZeroCiphertext
		}

	case ProofInstrVerifyCiphertextCiphertextEquality:
		{
			return zksdk.VerifyCiphertextCiphertextEquality
		}

	case ProofInstrVerifyCiphertextCommitmentEquality:
		{
			return zksdk.VerifyCiphertextCommitmentEquality
		}

	case ProofInstrVerifyPubkeyValidity:
		{
			return zksdk.VerifyPubkeyValidity
		}

	case ProofInstrVerifyPercentageWithCap:
		{
			return zksdk.VerifyPercentageWithCap
		}

	case ProofInstrVerifyBatchedRangeProofU64:
		{
			return zksdk.VerifyBatchedRangeProofU64
		}

	case ProofInstrVerifyBatchedRangeProofU128:
		{
			return zksdk.VerifyBatchedRangeProofU128
		}

	case ProofInstrVerifyBatchedRangeProofU256:
		{
			return zksdk.VerifyBatchedRangeProofU256
		}

	case ProofInstrVerifyGroupedCiphertext2HandlesValidity:
		{
			return zksdk.VerifyGroupedCiphertext2HandlesValidity
		}

	case ProofInstrVerifyBatchedGroupedCiphertext2HandlesValidity:
		{
			return zksdk.VerifyBatchedGroupedCiphertext2HandlesValidity
		}

	case ProofInstrVerifyGroupedCiphertext3HandlesValidity:
		{
			return zksdk.VerifyGroupedCiphertext3HandlesValidity
		}

	case ProofInstrVerifyBatchedGroupedCiphertext3HandlesValidity:
		{
			return zksdk.VerifyBatchedGroupedCiphertext3HandlesValidity
		}

	default:
		{
			panic("shouldn't be possible - programming error")
		}
	}
}

func processVerifyProof(execCtx *ExecutionCtx) error {
	txCtx := execCtx.TransactionContext
	instrCtx, err := txCtx.CurrentInstructionCtx()
	if err != nil {
		return err
	}

	instrData := instrCtx.Data
	instrType := instrCtx.Data[0]
	contextLen := ctxObjLens[instrType]
	proofDataLen := proofLens[instrType] + contextLen

	zkDispatchFn := getZkVerificationFn(instrType)

	var numAccessedAccts uint64
	var proofData []byte

	if len(instrData) == instrDataLenWithProofAcct {
		// case 1. proof data from account data
		proofDataAcct, err := instrCtx.BorrowInstructionAccount(txCtx, numAccessedAccts)
		if err != nil {
			return err
		}
		defer proofDataAcct.Drop()

		numAccessedAccts++

		proofDataOffset := binary.LittleEndian.Uint32(instrData[1:instrDataLenWithProofAcct])
		if uint64(proofDataOffset)+proofDataLen > uint64(len(proofDataAcct.Data())) {
			return InstrErrInvalidAccountData
		}

		proofData = make([]byte, proofDataLen)
		copy(proofData, proofDataAcct.Data()[proofDataOffset:])
		proofDataAcct.Drop()
	} else {
		// case 2. proof data from instruction data
		if uint64(len(instrData)) != (1 + proofDataLen) {
			return InstrErrInvalidInstructionData
		}
		proofData = instrData[1:]
	}

	err = zkDispatchFn(proofData)

	if instrCtx.NumberOfInstructionAccounts() > numAccessedAccts {
		contextStateAuthorityAcct, err := instrCtx.BorrowInstructionAccount(txCtx, numAccessedAccts+1)
		if err != nil {
			return err
		}
		contextStateAuthorityPubkey := contextStateAuthorityAcct.Key()

		proofContextAcct, err := instrCtx.BorrowInstructionAccount(txCtx, numAccessedAccts)
		if err != nil {
			return err
		}

		if proofContextAcct.Owner() != a.ZkElgamalProofProgramAddr {
			return InstrErrInvalidAccountOwner
		}

		if len(proofContextAcct.Data()) >= ctxHdrLen && proofContextAcct.Data()[32] != 0 {
			return InstrErrAccountAlreadyInitialized
		}

		contextBufferLen := ctxHdrLen + contextLen
		if uint64(len(proofContextAcct.Data())) != contextBufferLen {
			return InstrErrInvalidAccountData
		}

		buf := make([]byte, contextBufferLen)
		copy(buf, contextStateAuthorityPubkey[:])
		buf[32] = instrType

		if len(instrData) != 5 {
			copy(buf[ctxHdrLen:], proofData[:contextLen])
		}

		err = proofContextAcct.SetData(execCtx.GlobalCtx.Features, buf)
		if err != nil {
			return err
		}
	}

	return nil
}

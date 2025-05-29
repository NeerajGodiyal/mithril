package sealevel

import (
	"errors"

	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/gagliardetto/solana-go"
)

var IsPrecompile = errors.New("IsPrecompile")

var invalidEnumValue = errors.New("invalid enum value")

func resolveNativeProgramById(programId [32]byte) (func(ctx *ExecutionCtx) error, string, error) {

	switch programId {
	case a.SystemProgramAddr:
		return SystemProgramExecute, a.SystemProgramAddrStr, nil
	case a.StakeProgramAddr:
		return StakeProgramExecute, a.StakeProgramAddrStr, nil
	case a.VoteProgramAddr:
		return VoteProgramExecute, a.VoteProgramAddrStr, nil
	case a.ComputeBudgetProgramAddr:
		return ComputeBudgetExecute, a.ComputeBudgetProgramAddrStr, nil
	case a.BpfLoader2Addr:
		return BpfLoaderProgramExecute, a.BpfLoader2AddrStr, nil
	case a.BpfLoaderDeprecatedAddr:
		return BpfLoaderProgramExecute, a.BpfLoaderDeprecatedAddrStr, nil
	case a.BpfLoaderUpgradeableAddr:
		return BpfLoaderProgramExecute, a.BpfLoaderUpgradeableAddrStr, nil
	case a.ZkElgamalProofProgramAddr:
		return ElGamalExecute, a.ZkElgamalProofProgramAddrStr, nil
	case a.Ed25519PrecompileAddr:
		return Ed25519ProgramExecute, a.Ed25519PrecompileAddrStr, nil
	case a.Secp256kPrecompileAddr:
		return Secp256k1ProgramExecute, a.Secp256kPrecompileAddrStr, nil
	}

	return nil, "UNSUPPORTED", InstrErrUnsupportedProgramId
}

func verifySigner(authorized solana.PublicKey, signers []solana.PublicKey) error {
	for _, signer := range signers {
		if signer == authorized {
			return nil
		}
	}
	return InstrErrMissingRequiredSignature
}

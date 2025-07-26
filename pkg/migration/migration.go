package migration

import (
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/cu"
	"github.com/gagliardetto/solana-go"
)

var migratingProgramCus = map[solana.PublicKey]uint64{
	a.StakeProgramAddr: cu.CUStakeProgramDefaultComputeUnits,
}

var migratingBuiltins = map[solana.PublicKey]bool{
	a.StakeProgramAddr:       true,
	a.ConfigProgramAddr:      true,
	a.AddressLookupTableAddr: true,
}

var alreadyMigratedBuiltins = map[solana.PublicKey]bool{
	a.StakeProgramAddr:       true,
	a.ConfigProgramAddr:      true,
	a.AddressLookupTableAddr: true,
}

var nonMigratingBuiltins = map[solana.PublicKey]bool{
	a.VoteProgramAddr:          true,
	a.SystemProgramAddr:        true,
	a.ComputeBudgetProgramAddr: true,
	a.BpfLoaderUpgradeableAddr: true,
	a.BpfLoader2Addr:           true,
	a.BpfLoaderDeprecatedAddr:  true,
	a.LoaderV4Addr:             true,
	a.Secp256kPrecompileAddr:   true,
	a.Ed25519PrecompileAddr:    true,
}

func IsNonMigratingBuiltinProgram(pubkey solana.PublicKey) bool {
	_, isNonMigrating := nonMigratingBuiltins[pubkey]
	return isNonMigrating
}

func IsMigratingBuiltinProgram(pubkey solana.PublicKey) bool {
	_, isMigrating := migratingBuiltins[pubkey]
	return isMigrating
}

func HasBuiltinMigratedYet(pubkey solana.PublicKey) bool {
	_, hasAlreadyMigrated := alreadyMigratedBuiltins[pubkey]
	return hasAlreadyMigrated
}

func IsMigratingProgramAndGetCUs(programId solana.PublicKey) (uint64, bool) {
	cu, isMigrating := migratingProgramCus[programId]
	return cu, isMigrating
}

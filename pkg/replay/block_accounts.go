package replay

import (
	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

// SysvarAddresses returns all sysvar account addresses needed for replay.
func SysvarAddresses() []solana.PublicKey {
	return []solana.PublicKey{
		sealevel.SysvarClockAddr,
		sealevel.SysvarRentAddr,
		sealevel.SysvarEpochScheduleAddr,
		sealevel.SysvarFeesAddr,
		sealevel.SysvarSlotHashesAddr,
		sealevel.SysvarRecentBlockHashesAddr,
		sealevel.SysvarEpochRewardsAddr,
		sealevel.SysvarStakeHistoryAddr,
	}
}

// NativeProgramAddresses returns addresses of native programs that might be invoked.
func NativeProgramAddresses() []solana.PublicKey {
	return []solana.PublicKey{
		solana.PublicKey(a.SystemProgramAddr),
		solana.PublicKey(a.VoteProgramAddr),
		solana.PublicKey(a.StakeProgramAddr),
		solana.PublicKey(a.ComputeBudgetProgramAddr),
		solana.PublicKey(a.BpfLoader2Addr),
		solana.PublicKey(a.BpfLoaderDeprecatedAddr),
		solana.PublicKey(a.BpfLoaderUpgradeableAddr),
		solana.PublicKey(a.LoaderV4Addr),
		solana.PublicKey(a.Ed25519PrecompileAddr),
		solana.PublicKey(a.Secp256kPrecompileAddr),
		solana.PublicKey(a.Secp256r1PrecompileAddr),
		solana.PublicKey(a.ZkElgamalProofProgramAddr),
	}
}

// GetBlockAccountDependencies returns all account pubkeys needed to replay a block.
// This includes:
// - All accounts referenced in transaction AccountKeys
// - All accounts in TxMeta.LoadedAddresses (from ALTs)
// - All sysvar accounts
// - Native program addresses
//
// Note: ProgramData accounts for BPFLoaderUpgradeable programs must be resolved
// separately using ResolveProgramDataAccounts after fetching the program accounts.
func GetBlockAccountDependencies(b *block.Block) []solana.PublicKey {
	seen := make(map[solana.PublicKey]bool)
	var result []solana.PublicKey

	add := func(pk solana.PublicKey) {
		if !seen[pk] {
			seen[pk] = true
			result = append(result, pk)
		}
	}

	// Add sysvar accounts
	for _, pk := range SysvarAddresses() {
		add(pk)
	}

	// Add native program addresses
	for _, pk := range NativeProgramAddresses() {
		add(pk)
	}

	// Add all accounts from transactions
	for i, tx := range b.Transactions {
		// Static account keys from the transaction
		for _, pk := range tx.Message.AccountKeys {
			add(pk)
		}

		// Loaded addresses from ALT resolution (in TxMeta)
		if b.TxMetas != nil && i < len(b.TxMetas) && b.TxMetas[i] != nil {
			meta := b.TxMetas[i]
			for _, pk := range meta.LoadedAddresses.Writable {
				add(pk)
			}
			for _, pk := range meta.LoadedAddresses.ReadOnly {
				add(pk)
			}
		}
	}

	return result
}

// ResolveProgramDataAccounts finds ProgramData accounts for BPFLoaderUpgradeable programs.
// Takes the fetched accounts and returns additional ProgramData addresses that need to be fetched.
// The caller should call this after fetching the initial accounts, then fetch the returned
// ProgramData accounts.
func ResolveProgramDataAccounts(accounts map[solana.PublicKey][]byte) []solana.PublicKey {
	bpfLoaderUpgradeableAddr := solana.PublicKey(a.BpfLoaderUpgradeableAddr)
	var programDataAddrs []solana.PublicKey

	for pk, data := range accounts {
		// Check if this looks like an executable account owned by BPFLoaderUpgradeable
		// We need to parse the account to check owner and executable flag
		// For now, we'll try to parse any account that might be a program
		if len(data) < 36 {
			continue
		}

		// Try to parse as UpgradeableLoaderState
		state, err := sealevel.UnmarshalUpgradeableLoaderState(data)
		if err != nil {
			continue
		}

		// If it's a Program type, extract the ProgramData address
		if state.Type == sealevel.UpgradeableLoaderStateTypeProgram {
			programDataAddrs = append(programDataAddrs, state.Program.ProgramDataAddress)
		}

		_ = pk
		_ = bpfLoaderUpgradeableAddr
	}

	return programDataAddrs
}

// GetBlockAccountDependenciesWithOwners is like GetBlockAccountDependencies but also
// includes account owner information when available. This is useful for identifying
// which accounts are programs that need ProgramData resolution.
type AccountDependency struct {
	Pubkey     solana.PublicKey
	IsProgram  bool // True if this account is invoked as a program
}

// GetBlockAccountDependenciesDetailed returns account dependencies with metadata
// about whether each account is invoked as a program.
func GetBlockAccountDependenciesDetailed(b *block.Block) []AccountDependency {
	seen := make(map[solana.PublicKey]*AccountDependency)

	add := func(pk solana.PublicKey, isProgram bool) {
		if dep, exists := seen[pk]; exists {
			if isProgram {
				dep.IsProgram = true
			}
		} else {
			seen[pk] = &AccountDependency{
				Pubkey:    pk,
				IsProgram: isProgram,
			}
		}
	}

	// Add sysvar accounts (not programs)
	for _, pk := range SysvarAddresses() {
		add(pk, false)
	}

	// Add native program addresses (these are programs)
	for _, pk := range NativeProgramAddresses() {
		add(pk, true)
	}

	// Add all accounts from transactions
	for i, tx := range b.Transactions {
		// Static account keys from the transaction
		for _, pk := range tx.Message.AccountKeys {
			add(pk, false)
		}

		// Mark program IDs as programs
		for _, instr := range tx.Message.Instructions {
			if int(instr.ProgramIDIndex) < len(tx.Message.AccountKeys) {
				programId := tx.Message.AccountKeys[instr.ProgramIDIndex]
				add(programId, true)
			}
		}

		// Loaded addresses from ALT resolution (in TxMeta)
		if b.TxMetas != nil && i < len(b.TxMetas) && b.TxMetas[i] != nil {
			meta := b.TxMetas[i]
			for _, pk := range meta.LoadedAddresses.Writable {
				add(pk, false)
			}
			for _, pk := range meta.LoadedAddresses.ReadOnly {
				add(pk, false)
			}
		}
	}

	result := make([]AccountDependency, 0, len(seen))
	for _, dep := range seen {
		result = append(result, *dep)
	}
	return result
}

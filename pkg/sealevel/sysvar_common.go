package sealevel

import "github.com/Overclock-Validator/mithril/pkg/accounts"

func addrObjectForLookup(execCtx *ExecutionCtx) *accounts.Accounts {
	if execCtx == nil {
		return nil
	}
	// A transaction always observes the sysvars of the bank that executes it.
	// Leader banks are speculative (Replay is false), but PrepareLeaderSlotSysvars
	// still installs their bank-local Clock and SlotHashes in SlotCtx.Accounts.
	// Falling back to ExecutionCtx.Accounts here made sol_get_sysvar read the
	// process-global parent cache while forging, then the child-bank value during
	// ordered replay.
	if execCtx.SlotCtx != nil && execCtx.SlotCtx.Accounts != nil {
		return &execCtx.SlotCtx.Accounts
	}
	return &execCtx.Accounts
}

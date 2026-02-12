package sealevel

import "github.com/Overclock-Validator/mithril/pkg/accounts"

func addrObjectForLookup(execCtx *ExecutionCtx) *accounts.Accounts {
	if execCtx.AccountsForLookup != nil {
		return &execCtx.AccountsForLookup
	}
	return &execCtx.Accounts
}

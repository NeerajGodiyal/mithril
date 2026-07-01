package accounts

import (
	"maps"
	"sync"

	"github.com/gagliardetto/solana-go"
)

// OverlayAccounts is a branch-local MVCC overlay over a parent account set: writes
// go to an in-memory delta, reads fall back to the never-mutated parent.
type OverlayAccounts struct {
	mu     sync.RWMutex
	delta  map[[32]byte]*Account
	parent Accounts
}

func NewOverlayAccounts(parent Accounts) *OverlayAccounts {
	return &OverlayAccounts{
		delta:  make(map[[32]byte]*Account),
		parent: parent,
	}
}

func (o *OverlayAccounts) GetAccount(pubkey *[32]byte) (*Account, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if acct, ok := o.delta[*pubkey]; ok {
		return acct, nil
	}
	// Lock order is always overlay -> parent, so holding RLock across the
	// parent read closes the delta/parent race without risking a deadlock.
	return o.parent.GetAccount(pubkey)
}

func (o *OverlayAccounts) GetAccountWithoutLock(pubkey solana.PublicKey) (*Account, error) {
	if acct, ok := o.delta[pubkey]; ok {
		return acct, nil
	}
	return o.parent.GetAccountWithoutLock(pubkey)
}

func (o *OverlayAccounts) SetAccount(pubkey *[32]byte, acct *Account) error {
	o.mu.Lock()
	o.delta[*pubkey] = acct
	o.mu.Unlock()
	return nil
}

func (o *OverlayAccounts) SetAccountWithoutLock(pubkey solana.PublicKey, acct *Account) error {
	o.delta[pubkey] = acct
	return nil
}

// AllAccounts returns the parent set with this branch's delta applied on top — a
// point-in-time view (delta snapshotted under lock), not consistent under concurrent writes.
func (o *OverlayAccounts) AllAccounts() []*Account {
	o.mu.RLock()
	deltaCopy := make(map[[32]byte]*Account, len(o.delta))
	maps.Copy(deltaCopy, o.delta)
	o.mu.RUnlock()

	// Merge outside the overlay lock: parent set first, then delta shadows it.
	merged := make(map[[32]byte]*Account)
	for _, acct := range o.parent.AllAccounts() {
		merged[[32]byte(acct.Key)] = acct
	}
	maps.Copy(merged, deltaCopy)
	out := make([]*Account, 0, len(merged))
	for _, acct := range merged {
		out = append(out, acct)
	}
	return out
}

// DeltaAccounts returns the accounts changed on this overlay (the branch diff).
// Multi-branch (#14) promote-winner path; not used by the current linear tip.
func (o *OverlayAccounts) DeltaAccounts() []*Account {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make([]*Account, 0, len(o.delta))
	for _, acct := range o.delta {
		out = append(out, acct)
	}
	return out
}

var _ Accounts = (*OverlayAccounts)(nil)

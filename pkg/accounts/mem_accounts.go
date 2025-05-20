package accounts

import (
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/base58"
)

type MemAccounts struct {
	Map map[[32]byte]*Account
	mu  *sync.Mutex
}

func NewMemAccounts() MemAccounts {
	return MemAccounts{
		Map: make(map[[32]byte]*Account),
		mu:  &sync.Mutex{},
	}
}

func (m MemAccounts) GetAccount(pubkey *[32]byte) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.Map[*pubkey]
	if !ok {
		return nil, fmt.Errorf("no such account %s found", base58.Encode(pubkey[:]))
	}
	return acct, nil
}

func (m MemAccounts) SetAccount(pubkey *[32]byte, acct *Account) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Map[*pubkey] = acct
	return nil
}

func (m MemAccounts) AllAccounts() []*Account {
	m.mu.Lock()
	defer m.mu.Unlock()
	accts := make([]*Account, 0)

	for _, acct := range m.Map {
		accts = append(accts, acct)
	}

	return accts
}

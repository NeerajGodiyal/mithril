package blockprod

import (
	"sync"
)

// Controller owns the active leader WorkingBank for the forge sink.
type Controller struct {
	mu   sync.RWMutex
	bank *WorkingBank
}

func NewController() *Controller {
	return &Controller{}
}

func (c *Controller) WorkingBank() *WorkingBank {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bank
}

func (c *Controller) SetWorkingBank(bank *WorkingBank) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bank = bank
}

func (c *Controller) ClearWorkingBank() {
	c.SetWorkingBank(nil)
}

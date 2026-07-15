package replay

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/stretchr/testify/require"
)

func TestCacheFeesSysvar_MissingAccountNoPanic(t *testing.T) {
	prev := sealevel.SysvarCache.Fees
	t.Cleanup(func() { sealevel.SysvarCache.Fees = prev })
	sealevel.SysvarCache.Fees.Sysvar = nil
	sealevel.SysvarCache.Fees.Acct = nil

	require.NotPanics(t, func() { cacheFeesSysvar(nil) })
	require.Nil(t, sealevel.SysvarCache.Fees.Sysvar)
	require.Nil(t, sealevel.SysvarCache.Fees.Acct)
}

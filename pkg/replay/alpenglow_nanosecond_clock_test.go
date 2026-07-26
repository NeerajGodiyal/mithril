package replay

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestNanosecondClockAccountAddr(t *testing.T) {
	require.Equal(t, "HQcg2uM8uUqfRprvypQGcU7qucUZJY3odWjJVYce2a6C", NanosecondClockAccountAddr().String())
}

func TestEncodeNanosecondClockData(t *testing.T) {
	nanos := int64(1782235622348189724)
	data := encodeNanosecondClockData(nanos)
	require.Len(t, data, nanosecondClockDataLen)
	require.Equal(t, nanos, int64(binary.LittleEndian.Uint64(data)))
}

func TestAlpenglowNanosecondClockUsesDefaultRentAndRentEpochZero(t *testing.T) {
	slotCtx := &sealevel.SlotCtx{
		Slot: 3768180,
		Accounts: func() accounts.Accounts {
			accts := accounts.NewMemAccountsWithLen(1)
			return accts
		}(),
		AcctMapsMu:    &sync.Mutex{},
		ModifiedAccts: make(map[solana.PublicKey]bool),
		WritableAccts: make(map[solana.PublicKey]bool),
	}
	block := &b.Block{
		Slot:                    3768180,
		FooterProducerTimeNanos: 1782240542728820757,
	}
	require.NoError(t, updateAlpenglowNanosecondClockAccount(slotCtx, block))
	acct, err := slotCtx.GetAccount(NanosecondClockAccountAddr())
	require.NoError(t, err)
	require.Equal(t, uint64(0), acct.RentEpoch)
	defaultRent := sealevel.NewDefaultRentSysvar()
	require.Equal(t, defaultRent.MinimumBalance(nanosecondClockDataLen), acct.Lamports)
	require.Equal(t, encodeNanosecondClockData(1782240542728820757), acct.Data)
}

func TestAlpenglowFooterProducerTimeNanosPrefersFooterNanos(t *testing.T) {
	block := &b.Block{
		UnixTimestamp:           1,
		FooterProducerTimeNanos: 1782235622348189724,
	}
	nanos, ok, err := alpenglowFooterProducerTimeNanos(block)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(1782235622348189724), nanos)
}

func TestNanosecondClockBounds(t *testing.T) {
	const slotNanos = 400_000_000
	parent := int64(1782240542_000000000)

	// One-slot gap: upper = parent + 2*400ms, lower = parent + 1.
	lower, upper := NanosecondClockBounds(parent, slotNanos)
	require.Equal(t, parent+1, lower)
	require.Equal(t, parent+2*slotNanos, upper)

	// Multi-slot gap scales the upper bound.
	_, upper5 := NanosecondClockBounds(parent, 5*slotNanos)
	require.Equal(t, parent+2*5*slotNanos, upper5)
}

func TestSkewBlockProducerTimeNanosClampsBothEnds(t *testing.T) {
	const slotNanos = 400_000_000
	parent := int64(1782240542_000000000)
	lower, upper := NanosecondClockBounds(parent, slotNanos)

	// Wall clock far ahead (leader just caught up) clamps down to the upper bound.
	require.Equal(t, upper, SkewBlockProducerTimeNanos(parent, parent+10*slotNanos, slotNanos))

	// Wall clock behind/equal to parent clamps up to parent+1.
	require.Equal(t, lower, SkewBlockProducerTimeNanos(parent, parent, slotNanos))
	require.Equal(t, lower, SkewBlockProducerTimeNanos(parent, parent-slotNanos, slotNanos))

	// In-bounds value passes through unchanged.
	inBounds := parent + slotNanos
	require.Equal(t, inBounds, SkewBlockProducerTimeNanos(parent, inBounds, slotNanos))
}

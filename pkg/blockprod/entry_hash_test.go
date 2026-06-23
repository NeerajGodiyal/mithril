package blockprod

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestEntryBuilderAdvancesAlpenglowEntryChain(t *testing.T) {
	parent := solana.Hash{1, 2, 3}
	builder := NewEntryBuilder(costmodelDefaultLimits(), parent)

	entries, batchBytes := builder.Flush()
	require.Nil(t, entries)
	require.Zero(t, batchBytes)

	tickHash := turbine.AlpentickHash(parent)
	require.Equal(t, tickHash, turbine.AlpentickHash(builder.CurrentEntryHash()))
}

func costmodelDefaultLimits() costmodel.Limits {
	return costmodel.DefaultLimits()
}

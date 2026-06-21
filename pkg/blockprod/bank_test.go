package blockprod

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/costmodel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/Overclock-Validator/mithril/pkg/turbine"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type captureSink struct {
	batches [][]turbine.Entry
	bytes   []int
}

func (s *captureSink) OnEntryBatch(entries []turbine.Entry, batchBytes int) {
	s.batches = append(s.batches, append([]turbine.Entry(nil), entries...))
	s.bytes = append(s.bytes, batchBytes)
}

func TestWorkingBankForgesTransfer(t *testing.T) {
	sink := &captureSink{}
	env := NewTestEnv(TestEnvConfig{Sink: sink})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	assert.Equal(t, ForgeAccepted, result)
	assert.Equal(t, costmodel.ExceedNone, reason)
	assert.Equal(t, 1, env.Bank.EntryBuilder().PendingCount())
}

func TestWorkingBankDropsInvalidWire(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	result, _ := env.Bank.Forge([]byte{0x00})
	assert.Equal(t, ForgeDroppedParse, result)
}

func TestWorkingBankDropsWhenBlockCostExceeded(t *testing.T) {
	limits := costmodel.DefaultLimits()
	limits.BlockCost = 1
	env := NewTestEnv(TestEnvConfig{Limits: limits})
	defer env.Close()

	wire := txfixture.MustSignedTransferWire(0)
	result, reason := env.Bank.Forge(wire)
	assert.Equal(t, ForgeDroppedCost, result)
	assert.Equal(t, costmodel.ExceedBlockCost, reason)
}

func TestWorkingBankFlushesOnBatchLimit(t *testing.T) {
	sink := &captureSink{}
	limits := costmodel.DefaultLimits()
	limits.MaxBatchBytes = 300
	env := NewTestEnv(TestEnvConfig{Limits: limits, Sink: sink})
	defer env.Close()

	for seq := uint64(0); seq < 3; seq++ {
		wire := txfixture.MustSignedTransferWire(seq)
		result, _ := env.Bank.Forge(wire)
		require.Equal(t, ForgeAccepted, result)
	}

	assert.GreaterOrEqual(t, len(sink.batches), 1)
	totalTxns := 0
	for _, batch := range sink.batches {
		for _, entry := range batch {
			totalTxns += len(entry.Txns)
		}
	}
	assert.GreaterOrEqual(t, totalTxns, 1)
}

func TestEntryBuilderFlush(t *testing.T) {
	builder := NewEntryBuilder(costmodel.DefaultLimits(), solana.Hash{0xcd})
	wire := txfixture.MustSignedTransferWire(0)
	tx, err := solana.TransactionFromBytes(wire)
	require.NoError(t, err)
	_, _, _ = builder.Append(*tx, len(wire))

	entries, batchBytes := builder.Flush()
	require.Len(t, entries, 1)
	assert.Equal(t, 1, len(entries[0].Txns))
	assert.Greater(t, batchBytes, 0)
}

func TestControllerWorkingBank(t *testing.T) {
	controller := NewController()
	assert.Nil(t, controller.WorkingBank())

	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()
	controller.SetWorkingBank(env.Bank)
	assert.Equal(t, env.Bank, controller.WorkingBank())
}

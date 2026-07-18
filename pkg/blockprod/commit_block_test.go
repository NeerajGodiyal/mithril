package blockprod

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/tpu/txfixture"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestBuildLeaderBlockSignatureCountIsBankLocal(t *testing.T) {
	env := NewTestEnv(TestEnvConfig{})
	defer env.Close()

	for seq := uint64(0); seq < 5; seq++ {
		result, _ := env.Bank.Forge(txfixture.MustSignedTransferWire(seq))
		require.Equal(t, ForgeAccepted, result)
	}

	block := BuildLeaderBlock(LeaderBlockInput{
		Bank:            env.Bank,
		EpochSchedule:   &sealevel.SysvarEpochSchedule{SlotsPerEpoch: 54_000},
		ParentSlot:      41,
		ParentBankhash:  solana.Hash{1},
		PrevNumSigs:     1_750,
		PrevFeeGovernor: &sealevel.FeeRateGovernor{LamportsPerSignature: 5_000},
		EntryBlockhash:  solana.Hash{2},
	})

	require.Equal(t, uint64(1_750), block.PrevNumSignatures)
	require.Equal(t, uint64(5), block.NumSignatures)
}

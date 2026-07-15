package sealevel

import (
	"encoding/hex"
	"testing"

	a "github.com/Overclock-Validator/mithril/pkg/addresses"
	"github.com/Overclock-Validator/mithril/pkg/features"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

// This is the InitializeAccountV2 instruction that first exposed the missing
// SIMD-0387/0464 replay path on the Alpenglow community cluster (slot 3255498).
const liveInitializeAccountV2Hex = "100000001cfcd3afb8a41b3d856aa61f537005eb9244244d44df68c3e2d424470238ad6d1cfcd3afb8a41b3d856aa61f537005eb9244244d44df68c3e2d424470238ad6dafb3314713820341adf589b6d680e981a942dd6087ed8a035f58eac3ca560862ff09ef12f4b56f1ce95add377c5fbffe8986ffb5d7e43c7ea4fe9990e4a79dda8b025c608b7f4bb8b0a4b381b13d0b60a8bde0146be2f5afec1110949fd8c87e085fb31b504cfd3cee0aed8f6762c441ddd03ca2c3c9f568bdcf869fa033782f0693fda20ca1d741f415348bb9a0e70e45b66eb9dff1944c9796a574164654e689f1e676af8c8c5a9a6a6839d381fff210271027"

func TestVoteInitializeAccountV2LiveVector(t *testing.T) {
	data, err := hex.DecodeString(liveInitializeAccountV2Hex)
	require.NoError(t, err)
	require.Len(t, data, 248)
	require.Equal(t, uint32(VoteProgramInstrTypeInitializeAccountV2), uint32(data[0]))

	var init VoteInstrVoteInitV2
	require.NoError(t, init.UnmarshalWithDecoder(bin.NewBinDecoder(data[4:])))
	require.Equal(t, solana.MustPublicKeyFromBase58("2xA1yx8N6GkYPJZJic6KnXgatEkV6AUmvPfQmbpmtrXe"), init.NodePubkey)
	require.Equal(t, init.NodePubkey, init.AuthorizedVoter)
	require.Equal(t, uint16(10_000), init.InflationRewardsCommissionBps)
	require.Equal(t, uint16(10_000), init.BlockRevenueCommissionBps)

	voteAccount := solana.MustPublicKeyFromBase58("3nnRHoGeND987Cv2BDUW1po9dVBcDvy4wHsVFmPj4zhY")
	args := &VoterWithBLSArgs{
		BlsPubkeyCompressed:  init.AuthorizedVoterBLSPubkey,
		BlsProofOfPossession: init.AuthorizedVoterBLSProofOfPossession,
	}
	require.NoError(t, verifyVoteBLSProofOfPossession(voteAccount, args))

	badProof := *args
	badProof.BlsProofOfPossession[0] ^= 0xff
	require.ErrorIs(t, verifyVoteBLSProofOfPossession(voteAccount, &badProof), InstrErrInvalidArgument)
}

func TestVoteInitializeAccountV2RequiresAllFeatures(t *testing.T) {
	ft := features.NewFeaturesDefault()
	require.False(t, voteProgramInitializeAccountV2Enabled(*ft))

	for _, gate := range []features.FeatureGate{
		features.BlsPubkeyManagementInVoteAccount,
		features.CommissionRateInBasisPoints,
		features.CustomCommissionCollector,
		features.BlockRevenueSharing,
		features.VoteAccountInitializeV2,
	} {
		ft.EnableFeature(gate, 1)
	}
	require.True(t, voteProgramInitializeAccountV2Enabled(*ft))

	ft.DisableFeature(features.CustomCommissionCollector)
	require.False(t, voteProgramInitializeAccountV2Enabled(*ft))
}

func TestComputeBudgetTreatsVoteAsNonBuiltinWhenBLSActive(t *testing.T) {
	instructions := []Instruction{
		{ProgramId: a.SystemProgramAddr},
		{ProgramId: a.VoteProgramAddr},
	}

	ft := features.NewFeaturesDefault()
	ft.EnableFeature(features.ReserveMinimalCUsForBuiltinInstructions, 0)
	withoutBLS, err := ComputeBudgetExecuteInstructions(instructions, ft)
	require.NoError(t, err)
	require.Equal(t, uint32(2*MaxBuiltinAllocationComputeUnitLimit), withoutBLS.ComputeUnitLimit)

	ft.EnableFeature(features.BlsPubkeyManagementInVoteAccount, 0)
	withBLS, err := ComputeBudgetExecuteInstructions(instructions, ft)
	require.NoError(t, err)
	require.Equal(t, uint32(MaxBuiltinAllocationComputeUnitLimit+DefaultInstructionComputeUnitLimit), withBLS.ComputeUnitLimit)
}

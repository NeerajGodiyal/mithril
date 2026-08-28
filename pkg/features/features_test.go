package features

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/stretchr/testify/assert"
)

// The TestFflags_EnableAndDisable function tests that the
// enable and disable features work correctly.
func TestFflags_EnableAndDisable(t *testing.T) {
	f := NewFeaturesDefault()
	f.EnableFeature(StopTruncatingStringsInSyscalls, 0)
	assert.Equal(t, f.IsActive(StopTruncatingStringsInSyscalls), true)
	f.DisableFeature(StopTruncatingStringsInSyscalls)
	assert.Equal(t, f.IsActive(StopTruncatingStringsInSyscalls), false)
	f.EnableFeature(StopTruncatingStringsInSyscalls, 0)
	assert.Equal(t, f.IsActive(StopTruncatingStringsInSyscalls), true)
}

func TestFeatureActiveAtSlot(t *testing.T) {
	f := NewFeaturesDefault()
	f.EnableFeature(RaiseBlockLimitsTo100m, 10)
	assert.False(t, f.IsActiveAtSlot(RaiseBlockLimitsTo100m, 9))
	assert.True(t, f.IsActiveAtSlot(RaiseBlockLimitsTo100m, 10))
	assert.False(t, (*Features)(nil).IsActiveAtSlot(RaiseBlockLimitsTo100m, 10))
}

// The TestFflags_ListEnabled function tests that the AllEnabled function works
// as expected.
func TestFflags_ListEnabled(t *testing.T) {
	f := NewFeaturesDefault()
	f.EnableFeature(StopTruncatingStringsInSyscalls, 0)
	assert.Equal(t, f.AllEnabled(), []string{"feature StopTruncatingStringsInSyscalls (16FMCmgLzCNNz6eTwGanbyN2ZxvTBSLuQ6DZhgeMshg) enabled"})
}

func TestValidateChainedBlockIdFeatureGate(t *testing.T) {
	assert.Equal(t, "ValidateChainedBlockId", ValidateChainedBlockId.Name)
	assert.Equal(t, base58.MustDecodeFromString("vcmrbYbiMVKaq1snKP6eCacNDcr6qZvpCNUjmk6gxvZ"), ValidateChainedBlockId.Address)
	assert.Contains(t, AllFeatureGates, ValidateChainedBlockId)
}

func TestDiscardUnexpectedDataCompleteShredsFeatureGate(t *testing.T) {
	assert.Equal(t, "DiscardUnexpectedDataCompleteShreds", DiscardUnexpectedDataCompleteShreds.Name)
	assert.Equal(t, base58.MustDecodeFromString("dcomRRWHXP1FVWPqi9Mm4oxJhF4ehC795SvAtUdA9os"), DiscardUnexpectedDataCompleteShreds.Address)
	assert.Contains(t, AllFeatureGates, DiscardUnexpectedDataCompleteShreds)
}

func TestOfficialFixedFECFeatureGates(t *testing.T) {
	assert.Equal(t, base58.MustDecodeFromString("fixfecLZYMfkGzwq6NJA11Yw6KYztzXiK9QcL3K78in"), EnforceFixedFECSet.Address)
	assert.Equal(t, base58.MustDecodeFromString("disCA4efguFL6Wqa4pGdG7jpjC7C5uiKzKnhEBqchBe"), AgaveDiscardUnexpectedDataCompleteShreds.Address)
	assert.Contains(t, AllFeatureGates, EnforceFixedFECSet)
	assert.Contains(t, AllFeatureGates, AgaveDiscardUnexpectedDataCompleteShreds)
}

func TestDisableFeesSysvarFeatureGate(t *testing.T) {
	assert.Equal(t, "DisableFeesSysvar", DisableFeesSysvar.Name)
	assert.Equal(t, base58.MustDecodeFromString("JAN1trEUEtZjgXYzNBYHU9DYd7GnThhXfFP7SzPXkPsG"), DisableFeesSysvar.Address)
	assert.Contains(t, AllFeatureGates, DisableFeesSysvar)
}

func TestEnableSbpfV3DeploymentAndExecutionFeatureGates(t *testing.T) {
	assert.Equal(t, "EnableSbpfV3DeploymentAndExecution", EnableSbpfV3DeploymentAndExecution.Name)
	assert.Equal(t, base58.MustDecodeFromString("5cC3foj77CWun58pC51ebHFUWavHWKarWyR5UUik7dnC"), EnableSbpfV3DeploymentAndExecution.Address)
	assert.Contains(t, AllFeatureGates, EnableSbpfV3DeploymentAndExecution)

	f := NewFeaturesDefault()
	f.EnableFeature(EnableSbpfV3DeploymentAndExecution, 0)
	assert.True(t, f.IsSbpfV3DeploymentAndExecutionActive())
}

func TestAlpenglowFeatureGate(t *testing.T) {
	assert.Equal(t, "Alpenglow", Alpenglow.Name)
	assert.Equal(t, "A1PeNGc3D8SQmKwdYf4qj1XG7XgWVSuFQaiJSCQj775h", AlpenglowFeatureGateAddress)
	assert.Equal(t, base58.MustDecodeFromString(AlpenglowFeatureGateAddress), Alpenglow.Address)
	assert.Contains(t, AllFeatureGates, Alpenglow)
}

func TestAlpenglowVATAndSlotTimeFeatureGates(t *testing.T) {
	for _, gate := range []FeatureGate{
		ValidatorAdmissionTicket,
		ReduceSlotTimeTo350ms,
		ReduceSlotTimeTo300ms,
		ReduceSlotTimeTo250ms,
		ReduceSlotTimeTo200ms,
		RaiseBlockLimitsTo100m,
	} {
		assert.Contains(t, AllFeatureGates, gate)
	}
}

func TestVoteAccountInitializeV2FeatureGates(t *testing.T) {
	for _, gate := range []FeatureGate{
		BlsPubkeyManagementInVoteAccount,
		CommissionRateInBasisPoints,
		CustomCommissionCollector,
		BlockRevenueSharing,
		VoteAccountInitializeV2,
	} {
		assert.Contains(t, AllFeatureGates, gate)
	}
}

func TestCustomCommissionCollectorFeatureGate(t *testing.T) {
	assert.Equal(t, "CustomCommissionCollector", CustomCommissionCollector.Name)
	assert.Equal(
		t,
		base58.MustDecodeFromString("3HcSrCTGXTUnrTueHi4DAwNuMxZSsm5xui2Ax3mgxHqf"),
		CustomCommissionCollector.Address,
	)
	assert.Contains(t, AllFeatureGates, CustomCommissionCollector)
}

func TestSyscallParameterAddressRestrictionsFeatureGate(t *testing.T) {
	assert.Equal(t, "SyscallParameterAddressRestrictions", SyscallParameterAddressRestrictions.Name)
	assert.Equal(t, base58.MustDecodeFromString("EDGMC5kxFxGk4ixsNkGt8bW7QL5hDMXnbwaZvYMwNfzF"), SyscallParameterAddressRestrictions.Address)
	assert.Contains(t, AllFeatureGates, SyscallParameterAddressRestrictions)
}

func TestBlake3SyscallEnabledFeatureGate(t *testing.T) {
	assert.Equal(t, "Blake3SyscallEnabled", Blake3SyscallEnabled.Name)
	assert.Equal(t, base58.MustDecodeFromString("HTW2pSyErTj4BV6KBM9NZ9VBUJVxt7sacNWcf76wtzb3"), Blake3SyscallEnabled.Address)
	assert.Contains(t, AllFeatureGates, Blake3SyscallEnabled)
}

func TestVirtualAddressSpaceAdjustmentsFeatureGate(t *testing.T) {
	assert.Equal(t, "VirtualAddressSpaceAdjustments", VirtualAddressSpaceAdjustments.Name)
	assert.Equal(t, base58.MustDecodeFromString("7VgiehxNxu53KdxgLspGQY8myE6f7UokaWa4jsGcaSz"), VirtualAddressSpaceAdjustments.Address)
	assert.Contains(t, AllFeatureGates, VirtualAddressSpaceAdjustments)
}

func TestAccountDataDirectMappingFeatureGate(t *testing.T) {
	assert.Equal(t, "AccountDataDirectMapping", AccountDataDirectMapping.Name)
	assert.Equal(t, base58.MustDecodeFromString("CR3dVN2Yoo95Y96kLSTaziWDAQT2MNEpiWh5cqVq2pNE"), AccountDataDirectMapping.Address)
	assert.Contains(t, AllFeatureGates, AccountDataDirectMapping)
}

func TestDirectAccountPointersInProgramInputFeatureGate(t *testing.T) {
	assert.Equal(t, "DirectAccountPointersInProgramInput", DirectAccountPointersInProgramInput.Name)
	assert.Equal(t, base58.MustDecodeFromString("ptr9umikaeAS7ZBBp2fsfRhie16F1V2jCKA2y6gXNAK"), DirectAccountPointersInProgramInput.Address)
	assert.Contains(t, AllFeatureGates, DirectAccountPointersInProgramInput)
}

func TestTransactionV1FeatureGate(t *testing.T) {
	assert.Equal(t, "EnableTransactionV1", EnableTransactionV1.Name)
	assert.Equal(t, base58.MustDecodeFromString("txv1aq4pp281K9um3tnPgkfX8UqtFT6wcVW3hNezGLL"), EnableTransactionV1.Address)
	assert.Contains(t, AllFeatureGates, EnableTransactionV1)
}

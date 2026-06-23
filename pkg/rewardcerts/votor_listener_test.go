package rewardcerts

import (
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveVotorMessageRecordsSkipAndNotarVotes(t *testing.T) {
	builder := NewBuilder(DefaultBuilderConfig())
	slot := uint64(40)
	var blockID solana.Hash
	blockID[0] = 3
	keys := testBLSKeys(t, 2)

	skipVote := testSignedVote(t, alpenglow.NewSkipVote(slot), 0, keys[0])
	notarVote := testSignedVote(t, alpenglow.NewNotarizationVote(slot, blockID), 1, keys[1])
	finalizeVote := testSignedVote(t, alpenglow.NewFinalizationVote(slot), 0, keys[0])

	builder.ObserveVotorMessage(alpenglow.Message{Vote: &skipVote})
	builder.ObserveVotorMessage(alpenglow.Message{Vote: &notarVote})
	builder.ObserveVotorMessage(alpenglow.Message{Vote: &finalizeVote})

	certs := builder.BuildForLeaderSlot(slot + SlotsForReward)
	require.NotEmpty(t, certs.Skip)
	require.NotEmpty(t, certs.Notar)
}

func TestObserveVotorMessageIgnoresCertificates(t *testing.T) {
	builder := NewBuilder(DefaultBuilderConfig())
	builder.ObserveVotorMessage(alpenglow.NewCertificateMessage(alpenglow.Certificate{
		Type: alpenglow.CertificateSkip,
		Slot: 10,
	}))
	assert.Empty(t, builder.BuildForLeaderSlot(10+SlotsForReward).Skip)
}

func TestAddVotePrunesFromRootSlotFn(t *testing.T) {
	var root uint64 = 100
	builder := NewBuilder(BuilderConfig{RootSlot: func() uint64 { return root }})

	builder.AddVote(testSignedVote(t, alpenglow.NewSkipVote(80), 0, testBLSKeys(t, 1)[0]))
	root = 200
	builder.AddVote(testSignedVote(t, alpenglow.NewSkipVote(190), 0, testBLSKeys(t, 1)[0]))

	certs := builder.BuildForLeaderSlot(190 + SlotsForReward)
	require.NotEmpty(t, certs.Skip)
	decoded, err := DecodeSkipRewardCertificate(certs.Skip)
	require.NoError(t, err)
	assert.Equal(t, uint64(190), decoded.Slot)
}

func TestStartVotorListenerRejectsEmptyBind(t *testing.T) {
	_, err := StartVotorListener(t.Context(), NewBuilder(DefaultBuilderConfig()), "", 0)
	require.Error(t, err)
}

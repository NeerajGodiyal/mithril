package replay

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Overclock-Validator/mithril/pkg/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoundedReplayDivergence(t *testing.T) {
	safe := boundedReplayDivergence(&ReplayDivergence{
		TxIndex:     -5,
		TxSignature: strings.Repeat("s", 200),
		Kind:        "unexpected",
		Detail:      strings.Repeat("mismatch\n", 40),
	})

	require.NotNil(t, safe)
	assert.Equal(t, -1, safe.TxIndex)
	assert.Equal(t, "unknown", safe.Kind)
	assert.LessOrEqual(t, utf8.RuneCountInString(safe.TxSignature), maxDivergenceSignatureRunes)
	assert.LessOrEqual(t, utf8.RuneCountInString(safe.Detail), maxDivergenceDetailRunes)
	assert.NotContains(t, safe.Detail, "\n")
}

func TestActivePersistedVerificationDivergence(t *testing.T) {
	st := &state.MithrilState{LastRootedSlot: 10}
	assert.False(t, activePersistedVerificationDivergence(st, true))

	st.AlpenglowEvidence = append(st.AlpenglowEvidence, state.AlpenglowFinalityEvidence{Slot: 11})
	assert.True(t, activePersistedVerificationDivergence(st, true))
	assert.False(t, activePersistedVerificationDivergence(st, false))

	st.ReplayDivergenceEvidence = append(st.ReplayDivergenceEvidence, state.ReplayDivergenceRecord{Slot: 9})
	assert.True(t, activePersistedVerificationDivergence(st, false))
}

func TestReplayDivergenceEvidenceKeepsEarliestSlots(t *testing.T) {
	st := &state.MithrilState{}
	for slot := uint64(200); slot < 200+maxReplayDivergenceEvidence; slot++ {
		recordReplayDivergenceEvidence(st, &ReplayDivergence{Slot: slot, TxIndex: -1, Kind: "tx_count", Detail: "mismatch"})
	}
	recordReplayDivergenceEvidence(st, &ReplayDivergence{Slot: 100, TxIndex: -1, Kind: "tx_count", Detail: "earlier"})
	recordReplayDivergenceEvidence(st, &ReplayDivergence{Slot: 100, TxIndex: -1, Kind: "tx_count", Detail: "duplicate"})

	require.Len(t, st.ReplayDivergenceEvidence, maxReplayDivergenceEvidence)
	foundEarlier := false
	for _, record := range st.ReplayDivergenceEvidence {
		if record.Slot == 100 {
			foundEarlier = true
			assert.Equal(t, "earlier", record.Detail)
		}
		assert.NotEqual(t, uint64(263), record.Slot)
	}
	assert.True(t, foundEarlier)
}

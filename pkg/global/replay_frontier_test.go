package global

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplayFrontierIsIndependentFromBankSlot(t *testing.T) {
	SetSlot(40)
	SetReplayFrontier(42)
	require.Equal(t, uint64(40), Slot())
	require.Equal(t, uint64(42), ReplayFrontier())
}

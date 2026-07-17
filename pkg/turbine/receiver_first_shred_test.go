package turbine

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUDPReceiverNotifiesFirstShredOncePerSlot(t *testing.T) {
	receiver := NewUDPReceiver("127.0.0.1:0")
	var slots []uint64
	receiver.SetFirstShredSink(func(slot uint64) { slots = append(slots, slot) })
	receiver.noteFirstShred(41)
	receiver.noteFirstShred(41)
	receiver.noteFirstShred(42)
	require.Equal(t, []uint64{41, 42}, slots)
}

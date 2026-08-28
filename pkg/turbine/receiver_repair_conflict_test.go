package turbine

import (
	"context"
	"testing"

	"github.com/Overclock-Validator/mithril/fixtures"
)

func TestMetadataConflictCancelsSlotRepair(t *testing.T) {
	const slot = uint64(102815960)
	client := newPacingTestClient(t)
	key := repairRequestKey{kind: repairRequestWindowIndex, slot: slot, index: 999}
	client.outstanding[key] = outstandingRepairRequest{key: key}
	client.inflight[shredKey{kind: key.kind, slot: key.slot, index: key.index}] = &shredInflight{concurrent: 1}

	receiver := NewUDPReceiver("127.0.0.1:0")
	receiver.SetAlpenglowMode(true)
	receiver.repairClient = client
	packets := fixtures.DataShreds(t, "mainnet", slot)
	if len(packets) < 6 {
		t.Fatal("fixture needs at least six data shreds")
	}
	first := append([]byte(nil), packets[4]...)
	second := append([]byte(nil), packets[5]...)
	first[dataFlagsOffset] = shredFlagLastShredInSlot
	second[dataFlagsOffset] = shredFlagLastShredInSlot

	requireProcessPacket(t, receiver, first)
	requireProcessPacket(t, receiver, second)
	if len(client.outstanding) != 0 || len(client.inflight) != 0 {
		t.Fatalf("metadata-conflicted slot retained repair work: outstanding=%d inflight=%d", len(client.outstanding), len(client.inflight))
	}
}

func requireProcessPacket(t *testing.T, receiver *UDPReceiver, packet []byte) {
	t.Helper()
	if !receiver.processPacket(context.Background(), nil, packet, nil, false) {
		t.Fatal("receiver stopped while processing packet")
	}
}

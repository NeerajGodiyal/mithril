package gossip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Golden-byte test pinning the match-everything CrdsFilter to the exact agave
// bincode layout (verified against agave master). A drift here = silent no-op
// (peer ignores the request), so this must stay exact.
func TestEncodeCrdsFilterMatchAllGolden(t *testing.T) {
	var e encoder
	encodeCrdsFilterMatchAll(&e, 0x1122334455667788)
	got := e.bytes()

	le := binary.LittleEndian
	want := make([]byte, 0, 57)
	want = le.AppendUint64(want, 1)                  // keys len
	want = le.AppendUint64(want, 0x1122334455667788) // keys[0]
	want = append(want, 0x01)                        // bits Option = Some
	want = le.AppendUint64(want, 1)                  // block count
	want = le.AppendUint64(want, 0)                  // block[0]
	want = le.AppendUint64(want, 64)                 // bit length
	want = le.AppendUint64(want, 0)                  // num_bits_set
	want = le.AppendUint64(want, ^uint64(0))         // mask
	want = le.AppendUint32(want, 0)                  // mask_bits

	if !bytes.Equal(got, want) {
		t.Fatalf("CrdsFilter bytes drift:\n got=%x\nwant=%x", got, want)
	}
	if len(got) != 61 {
		t.Fatalf("filter len = %d, want 61", len(got))
	}
}

// The full pull-request packet must lead with the PullRequest protocol tag (0)
// and stay within one UDP packet.
func TestEncodePullRequestFraming(t *testing.T) {
	value := CrdsValue{} // zero value: encodes signature(64) + data; enough to frame-check
	pkt, err := encodePullRequest(value, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkt) < 4 || binary.LittleEndian.Uint32(pkt[:4]) != protocolPullRequest {
		t.Fatalf("packet must start with PullRequest tag 0, got % x", pkt[:4])
	}
	if len(pkt) > packetDataSize {
		t.Fatalf("packet %d exceeds UDP limit %d", len(pkt), packetDataSize)
	}
	// tag(4)+filter(61)=65; the CrdsValue follows
	if len(pkt) <= 61 {
		t.Fatalf("packet too short to carry a CrdsValue after the filter: %d", len(pkt))
	}
}

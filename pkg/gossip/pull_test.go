package gossip

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Golden-byte test pinning the partitioned CrdsFilter to the exact current
// Agave wincode layout. A drift here means peers silently ignore the request.
func TestEncodeCrdsFilterPartitionGolden(t *testing.T) {
	var e encoder
	encodeCrdsFilterPartition(&e, 0x1122334455667788, 3)
	got := e.bytes()

	le := binary.LittleEndian
	want := make([]byte, 0, 57)
	want = le.AppendUint64(want, 1)                           // keys len
	want = le.AppendUint64(want, 0x1122334455667788)          // keys[0]
	want = append(want, 0x01)                                 // bits Option = Some
	want = le.AppendUint64(want, 1)                           // block count
	want = le.AppendUint64(want, 0)                           // block[0]
	want = le.AppendUint64(want, 64)                          // bit length
	want = le.AppendUint64(want, 0)                           // num_bits_set
	want = le.AppendUint64(want, uint64(3)<<58|^uint64(0)>>6) // mask
	want = le.AppendUint32(want, crdsPullMaskBits)            // mask_bits

	if !bytes.Equal(got, want) {
		t.Fatalf("CrdsFilter bytes drift:\n got=%x\nwant=%x", got, want)
	}
	if len(got) != 61 {
		t.Fatalf("filter len = %d, want 61", len(got))
	}
}

func TestCrdsPullPartitionsCoverHashSpace(t *testing.T) {
	seen := make(map[uint64]struct{}, crdsPullPartitions)
	for partition := uint64(0); partition < crdsPullPartitions; partition++ {
		var e encoder
		encodeCrdsFilterPartition(&e, partition, partition)
		filter := e.bytes()
		if len(filter) != 61 {
			t.Fatalf("partition %d filter len = %d", partition, len(filter))
		}
		mask := binary.LittleEndian.Uint64(filter[49:57])
		maskBits := binary.LittleEndian.Uint32(filter[57:61])
		if maskBits != crdsPullMaskBits {
			t.Fatalf("partition %d mask bits = %d", partition, maskBits)
		}
		seen[mask] = struct{}{}
	}
	if len(seen) != int(crdsPullPartitions) {
		t.Fatalf("unique masks = %d, want %d", len(seen), crdsPullPartitions)
	}
}

// The full pull-request packet must lead with the PullRequest protocol tag (0)
// and stay within one UDP packet.
func TestEncodePullRequestFraming(t *testing.T) {
	value := CrdsValue{} // zero value: encodes signature(64) + data; enough to frame-check
	pkt, err := encodePullRequest(value, 42, 0)
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

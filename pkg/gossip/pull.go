package gossip

import "fmt"

// Outbound CRDS PullRequest: asks a peer for one partition of its CRDS table so
// a fresh node bootstraps its peer/repair set. The byte layout is pinned by
// pull_test.go's golden test.

// Current Agave rejects incoming pull filters with fewer than six mask bits.
// Sixty-four partitions cover the complete hash space while keeping each reply
// bounded. An empty Bloom filter asks for every value in the selected partition.
const (
	crdsPullMaskBits   = uint32(6)
	crdsPullPartitions = uint64(1) << crdsPullMaskBits
)

func encodeCrdsFilterPartition(e *encoder, seed, partition uint64) {
	// Bloom.keys: Vec<u64> = len + one seed
	e.u64(1)
	e.u64(seed)
	// Bloom.bits: BitVec<u64> = Option<Box<[u64]>> Some + one zero block, then bit-len
	e.u8(1)   // Option = Some
	e.u64(1)  // block count
	e.u64(0)  // block[0] = no bits set
	e.u64(64) // bit length
	// Bloom.num_bits_set
	e.u64(0)
	// CrdsFilter.mask + mask_bits
	partition %= crdsPullPartitions
	mask := partition<<(64-crdsPullMaskBits) | ^uint64(0)>>crdsPullMaskBits
	e.u64(mask)
	e.u32(crdsPullMaskBits)
}

// encodePullRequest builds Protocol::PullRequest(partition filter, our ContactInfo).
func encodePullRequest(value CrdsValue, seed, partition uint64) ([]byte, error) {
	var e encoder
	e.variant(protocolPullRequest)
	encodeCrdsFilterPartition(&e, seed, partition)
	value.encode(&e)
	if len(e.bytes()) > packetDataSize {
		return nil, fmt.Errorf("pull request size %d exceeds packet size %d", len(e.bytes()), packetDataSize)
	}
	return e.bytes(), nil
}

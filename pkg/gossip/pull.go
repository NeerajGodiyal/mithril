package gossip

import "fmt"

// Outbound CRDS PullRequest: asks a peer to dump its CRDS table so a fresh node
// bootstraps its peer/repair set (without it, discovery stalls at ~1 peer). The
// byte layout is pinned by pull_test.go's golden test.

// encodeCrdsFilterMatchAll writes a filter that matches everything, so the peer
// returns its whole table. An empty keys list degenerates to "I have everything"
// (returns nothing), so keys holds one seed over all-zero bits.
func encodeCrdsFilterMatchAll(e *encoder, seed uint64) {
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
	e.u64(^uint64(0)) // mask = u64::MAX (single partition = whole hash space)
	e.u32(0)          // mask_bits = 0
}

// encodePullRequest builds Protocol::PullRequest(match-all filter, our ContactInfo).
func encodePullRequest(value CrdsValue, seed uint64) ([]byte, error) {
	var e encoder
	e.variant(protocolPullRequest)
	encodeCrdsFilterMatchAll(&e, seed)
	value.encode(&e)
	if len(e.bytes()) > packetDataSize {
		return nil, fmt.Errorf("pull request size %d exceeds packet size %d", len(e.bytes()), packetDataSize)
	}
	return e.bytes(), nil
}

package dedup

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestCacheSeenAndDuplicate(t *testing.T) {
	c := NewCache(4)

	if c.Seen(1) {
		t.Fatal("first insert should not be duplicate")
	}
	if !c.Seen(1) {
		t.Fatal("second insert should be duplicate")
	}
}

func TestCacheEvictsLRU(t *testing.T) {
	c := NewCache(2)

	if c.Seen(1) {
		t.Fatal("hash 1 should be new")
	}
	if c.Seen(2) {
		t.Fatal("hash 2 should be new")
	}
	if !c.Seen(1) {
		t.Fatal("hash 1 should still be cached")
	}
	if c.Seen(3) {
		t.Fatal("hash 3 should be new and evict hash 2")
	}
	if !c.Seen(1) {
		t.Fatal("hash 1 should remain cached as MRU")
	}
	if c.Seen(2) {
		t.Fatal("hash 2 should have been evicted")
	}
}

func TestCacheTouchUpdatesEvictionOrder(t *testing.T) {
	c := NewCache(2)

	c.Seen(10)
	c.Seen(20)
	if !c.Seen(10) {
		t.Fatal("touch hash 10")
	}
	c.Seen(30)
	if !c.Seen(10) {
		t.Fatal("hash 10 should remain cached")
	}
	// Check the retained entry first because a Seen miss inserts and can evict
	// another key from this two-entry cache.
	if c.Seen(20) {
		t.Fatal("hash 20 should have been evicted")
	}
}

func TestCacheRetainsRecentEntriesAfterSaturation(t *testing.T) {
	c := NewCache(2)

	for _, hash := range []uint64{1, 2, 3, 4} {
		if c.Seen(hash) {
			t.Fatalf("hash %d should be new", hash)
		}
	}
	// The intended LRU contents are now {3, 4}. The old insertion path made
	// every newly inserted node both head and tail, so inserting 4 evicted 3.
	if !c.Seen(3) {
		t.Fatal("recent hash 3 was lost after cache saturation")
	}
}

func TestCacheEvictionPreservesProbeChain(t *testing.T) {
	c := NewCache(2)

	// Capacity two gets a 16-bucket table; 1 and 17 have the same home bucket.
	// Evicting 1 must not leave the displaced 17 behind an empty bucket.
	for _, hash := range []uint64{1, 17, 5} {
		if c.Seen(hash) {
			t.Fatalf("hash %d should be new", hash)
		}
	}
	if !c.Seen(17) {
		t.Fatal("colliding hash became unreachable after eviction")
	}
}

func TestCacheInvariantsAcrossRollover(t *testing.T) {
	c := NewCache(4)
	sequence := []uint64{1, 2, 3, 4, 2, 5, 6, 5, 7, 3, 8, 7, 9}
	for step, hash := range sequence {
		c.Seen(hash)
		assertCacheInvariants(t, c, fmt.Sprintf("step %d hash %d", step, hash))
	}
}

func TestCacheRandomizedAgainstReferenceLRU(t *testing.T) {
	const operations = 20_000

	for _, capacity := range []int{1, 2, 3, 4, 7, 16} {
		t.Run(fmt.Sprintf("capacity_%d", capacity), func(t *testing.T) {
			c := NewCache(capacity)
			rng := rand.New(rand.NewSource(int64(0x5eed + capacity)))

			// Every key has the same home bucket, while the working set is large
			// enough to force frequent touches, evictions, and probe-cluster rebuilds.
			keyspace := make([]uint64, capacity*3+5)
			stride := uint64(c.mask) + 1
			for i := range keyspace {
				keyspace[i] = 1 + uint64(i)*stride
			}

			// The reference model stores each resident key's last-access sequence.
			// The smallest sequence is the key a correct LRU must evict next.
			recency := make(map[uint64]int, capacity)
			for step := 1; step <= operations; step++ {
				keyIndex := rng.Intn(len(keyspace))
				if rng.Intn(4) != 0 {
					keyIndex = rng.Intn(capacity + 2)
				}
				hash := keyspace[keyIndex]

				_, wantHit := recency[hash]
				if !wantHit && len(recency) == capacity {
					var oldestHash uint64
					oldestStep := operations + 1
					for residentHash, lastSeen := range recency {
						if lastSeen < oldestStep {
							oldestHash = residentHash
							oldestStep = lastSeen
						}
					}
					delete(recency, oldestHash)
				}
				recency[hash] = step

				if gotHit := c.Seen(hash); gotHit != wantHit {
					t.Fatalf("step %d hash %d: Seen=%t want %t", step, hash, gotHit, wantHit)
				}

				where := fmt.Sprintf("step %d hash %d", step, hash)
				assertCacheInvariants(t, c, where)
				if gotSize := int(c.size); gotSize != len(recency) {
					t.Fatalf("%s: size=%d want %d", where, gotSize, len(recency))
				}
				for _, candidate := range keyspace {
					_, _, gotMember := c.htabFind(candidate)
					_, wantMember := recency[candidate]
					if gotMember != wantMember {
						t.Fatalf("%s: membership for hash %d=%t want %t", where, candidate, gotMember, wantMember)
					}
				}
			}
		})
	}
}

func assertCacheInvariants(t *testing.T, c *Cache, where string) {
	t.Helper()
	if c.size > c.cap {
		t.Fatalf("%s: size %d exceeds capacity %d", where, c.size, c.cap)
	}
	if c.size == 0 {
		if c.head != invalidIdx || c.tail != invalidIdx {
			t.Fatalf("%s: empty cache has head=%d tail=%d", where, c.head, c.tail)
		}
		return
	}
	if c.head == invalidIdx || c.tail == invalidIdx {
		t.Fatalf("%s: non-empty cache has head=%d tail=%d", where, c.head, c.tail)
	}
	if c.nodes[c.head].prev != invalidIdx {
		t.Fatalf("%s: head %d has prev %d", where, c.head, c.nodes[c.head].prev)
	}
	if c.nodes[c.tail].next != invalidIdx {
		t.Fatalf("%s: tail %d has next %d", where, c.tail, c.nodes[c.tail].next)
	}

	reachable := make(map[uint32]struct{}, c.size)
	prev := invalidIdx
	idx := c.head
	for idx != invalidIdx {
		if idx >= c.size {
			t.Fatalf("%s: list index %d outside size %d", where, idx, c.size)
		}
		if _, duplicate := reachable[idx]; duplicate {
			t.Fatalf("%s: cycle at node %d", where, idx)
		}
		reachable[idx] = struct{}{}
		n := c.nodes[idx]
		if n.prev != prev {
			t.Fatalf("%s: node %d prev=%d want %d", where, idx, n.prev, prev)
		}
		if c.htab[n.htabSlot] != idx+1 {
			t.Fatalf("%s: node %d table slot %d contains %d", where, idx, n.htabSlot, c.htab[n.htabSlot])
		}
		slot, foundIdx, ok := c.htabFind(n.hash)
		if !ok || foundIdx != idx || slot != n.htabSlot {
			t.Fatalf("%s: node %d hash lookup=(slot=%d idx=%d ok=%t), want slot=%d", where, idx, slot, foundIdx, ok, n.htabSlot)
		}
		prev = idx
		idx = n.next
	}
	if uint32(len(reachable)) != c.size {
		t.Fatalf("%s: list reaches %d nodes, want %d", where, len(reachable), c.size)
	}
	if prev != c.tail {
		t.Fatalf("%s: list tail=%d, cache tail=%d", where, prev, c.tail)
	}

	tableEntries := 0
	for _, entry := range c.htab {
		if entry == 0 {
			continue
		}
		tableEntries++
		idx := entry - 1
		if _, ok := reachable[idx]; !ok {
			t.Fatalf("%s: table references unreachable node %d", where, idx)
		}
	}
	if uint32(tableEntries) != c.size {
		t.Fatalf("%s: table contains %d entries, want %d", where, tableEntries, c.size)
	}
}

func TestNewCachePreallocates(t *testing.T) {
	c := NewCache(128)
	if len(c.nodes) != 128 {
		t.Fatalf("nodes len=%d want 128", len(c.nodes))
	}
	if len(c.htab) < 256 {
		t.Fatalf("htab len=%d want >= 256", len(c.htab))
	}
}

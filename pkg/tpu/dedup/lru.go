package dedup

// DefaultCacheCapacity is the default bounded LRU dedup table size.
const DefaultCacheCapacity = 1 << 20

const invalidIdx = ^uint32(0)

type node struct {
	hash     uint64
	prev     uint32
	next     uint32
	htabSlot uint32
}

// Cache is a fixed-capacity LRU dedup table keyed by uint64 signature hashes.
// All storage is preallocated; Seen does not allocate on the hot path.
type Cache struct {
	cap   uint32
	size  uint32
	mask  uint32
	head  uint32
	tail  uint32
	htab  []uint32 // node index + 1; 0 means empty
	nodes []node
}

// NewCache returns a preallocated LRU cache for capacity distinct hashes.
func NewCache(capacity int) *Cache {
	if capacity <= 0 {
		capacity = DefaultCacheCapacity
	}
	cap32 := uint32(capacity)
	htabLen := htabSize(cap32)
	c := &Cache{
		cap:   cap32,
		mask:  htabLen - 1,
		head:  invalidIdx,
		tail:  invalidIdx,
		htab:  make([]uint32, htabLen),
		nodes: make([]node, cap32),
	}
	for i := range c.nodes {
		c.nodes[i].prev = invalidIdx
		c.nodes[i].next = invalidIdx
	}
	return c
}

func htabSize(capacity uint32) uint32 {
	n := capacity * 2
	if n < 16 {
		return 16
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	return n + 1
}

// Seen reports whether hash is already in the cache.
// On false it records hash as most-recently-used.
func (c *Cache) Seen(hash uint64) bool {
	slot, idx, ok := c.htabFind(hash)
	if ok {
		c.touch(idx)
		return true
	}

	var nodeIdx uint32
	if c.size < c.cap {
		nodeIdx = c.size
		c.size++
	} else {
		nodeIdx = c.evictTail()
		c.deleteHtabSlot(c.nodes[nodeIdx].htabSlot)
		slot = c.htabFindEmpty(hash)
	}

	n := &c.nodes[nodeIdx]
	n.hash = hash
	n.prev = invalidIdx
	n.next = invalidIdx
	n.htabSlot = slot
	c.htab[slot] = nodeIdx + 1
	c.linkHead(nodeIdx)
	return false
}

// deleteHtabSlot removes one open-addressing table entry and rebuilds the
// following probe cluster. Simply zeroing the slot would make entries displaced
// past it unreachable because htabFind stops at the first empty slot.
func (c *Cache) deleteHtabSlot(slot uint32) {
	c.htab[slot] = 0
	for next := (slot + 1) & c.mask; c.htab[next] != 0; next = (next + 1) & c.mask {
		entry := c.htab[next]
		c.htab[next] = 0
		idx := entry - 1
		newSlot := c.htabFindEmpty(c.nodes[idx].hash)
		c.htab[newSlot] = entry
		c.nodes[idx].htabSlot = newSlot
	}
}

func (c *Cache) htabFind(hash uint64) (slot uint32, idx uint32, ok bool) {
	slot = uint32(hash>>32^hash) & c.mask
	for {
		entry := c.htab[slot]
		if entry == 0 {
			return slot, 0, false
		}
		idx = entry - 1
		if c.nodes[idx].hash == hash {
			return slot, idx, true
		}
		slot = (slot + 1) & c.mask
	}
}

func (c *Cache) htabFindEmpty(hash uint64) uint32 {
	slot := uint32(hash>>32^hash) & c.mask
	for c.htab[slot] != 0 {
		slot = (slot + 1) & c.mask
	}
	return slot
}

func (c *Cache) touch(idx uint32) {
	if c.head == idx {
		return
	}

	n := &c.nodes[idx]
	if n.prev != invalidIdx {
		c.nodes[n.prev].next = n.next
	} else {
		c.head = n.next
	}
	if n.next != invalidIdx {
		c.nodes[n.next].prev = n.prev
	} else {
		c.tail = n.prev
	}

	c.linkHead(idx)
}

// linkHead links an unlinked node as the most recently used entry. New nodes
// cannot go through touch: its detach step would mistake their sentinel links
// for membership at the tail and corrupt the list.
func (c *Cache) linkHead(idx uint32) {
	n := &c.nodes[idx]
	n.prev = invalidIdx
	n.next = c.head
	if c.head != invalidIdx {
		c.nodes[c.head].prev = idx
	} else {
		c.tail = idx
	}
	c.head = idx
}

func (c *Cache) evictTail() uint32 {
	idx := c.tail
	n := &c.nodes[idx]
	c.tail = n.prev
	if c.tail != invalidIdx {
		c.nodes[c.tail].next = invalidIdx
	} else {
		c.head = invalidIdx
	}
	n.prev = invalidIdx
	n.next = invalidIdx
	return idx
}

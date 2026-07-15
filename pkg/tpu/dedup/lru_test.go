package dedup

import "testing"

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
	if c.Seen(20) {
		t.Fatal("hash 20 should have been evicted")
	}
	if !c.Seen(10) {
		t.Fatal("hash 10 should remain cached")
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

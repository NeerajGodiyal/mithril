package arena

import (
	"sync"
	"testing"
)

func TestResetHighLowHighReuseIsZeroed(t *testing.T) {
	const capacity = 128
	a := New[byte](capacity)

	full, ok := a.AllocN(capacity)
	if !ok {
		t.Fatal("full allocation unexpectedly fell back to the heap")
	}
	for i := range full {
		full[i] = 0xff
	}
	a.Reset()

	one, ok := a.AllocN(1)
	if !ok {
		t.Fatal("small allocation unexpectedly fell back to the heap")
	}
	if one[0] != 0 {
		t.Fatalf("small allocation contains stale byte %#x", one[0])
	}
	one[0] = 0x7f
	a.Reset()

	full, ok = a.AllocN(capacity)
	if !ok {
		t.Fatal("second full allocation unexpectedly fell back to the heap")
	}
	for i, value := range full {
		if value != 0 {
			t.Fatalf("reused byte %d contains stale value %#x", i, value)
		}
	}
}

func TestResetAfterConcurrentAllocationPhase(t *testing.T) {
	const (
		workers          = 8
		itemsPerWorker   = 128
		totalAllocations = workers * itemsPerWorker
	)
	a := New[resetFixture](totalAllocations)

	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := range workers {
		go func() {
			defer wg.Done()
			for item := range itemsPerWorker {
				allocated, ok := a.Alloc()
				if !ok {
					t.Errorf("worker %d allocation %d fell back to the heap", worker, item)
					return
				}
				allocated.value = uint64(worker + 1)
			}
		}()
	}
	wg.Wait()

	a.Reset()

	if a.index != 0 {
		t.Fatalf("index after reset = %d, want 0", a.index)
	}
	for i, item := range a.pool {
		if item != (resetFixture{}) {
			t.Fatalf("concurrently allocated slot %d was not cleared: %+v", i, item)
		}
	}
}

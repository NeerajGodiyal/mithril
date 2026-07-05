package turbine

import (
	"testing"
)

// Round-trip: appended packets come back whole; the floor deletes below;
// the byte cap drops the HIGHEST slots first (the live edge is cheap to
// re-fetch; the low end borders replay).
func TestShredSpoolRoundTrip(t *testing.T) {
	spool, err := OpenShredSpool(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer spool.Close()

	spool.Append(100, []byte("alpha"))
	spool.Append(100, []byte("beta"))
	spool.Append(101, []byte("gamma"))

	if !spool.HasSlot(100) || !spool.HasSlot(101) || spool.HasSlot(102) {
		t.Fatalf("HasSlot bookkeeping wrong")
	}
	got, err := spool.ReadSlot(100)
	if err != nil || len(got) != 2 || string(got[0]) != "alpha" || string(got[1]) != "beta" {
		t.Fatalf("ReadSlot(100) = %v, %v", got, err)
	}
	if slots := spool.SlotsInRange(100, 101); len(slots) != 2 || slots[0] != 100 || slots[1] != 101 {
		t.Fatalf("SlotsInRange = %v", slots)
	}

	spool.SetFloor(101)
	if spool.HasSlot(100) {
		t.Fatalf("floor must delete below")
	}
	spool.Append(99, []byte("stale"))
	if spool.HasSlot(99) {
		t.Fatalf("appends below the floor must be ignored")
	}
}

func TestShredSpoolCapDropsHighest(t *testing.T) {
	spool, err := OpenShredSpool(t.TempDir(), 64)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer spool.Close()

	payload := make([]byte, 20)
	spool.Append(10, payload) // 24 bytes
	spool.Append(11, payload) // 48
	spool.Append(12, payload) // 72 > 64 -> drop highest (12)
	if spool.HasSlot(12) {
		t.Fatalf("cap must drop the highest slot")
	}
	if !spool.HasSlot(10) || !spool.HasSlot(11) {
		t.Fatalf("low slots must survive the cap")
	}
}

// A restart adopts leftover slot files: they hydrate instead of re-repairing.
func TestShredSpoolAdoptsExistingFiles(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Append(500, []byte("persisted"))
	first.Close()

	second, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if !second.HasSlot(500) {
		t.Fatalf("reopen must adopt existing slot files")
	}
	got, err := second.ReadSlot(500)
	if err != nil || len(got) != 1 || string(got[0]) != "persisted" {
		t.Fatalf("ReadSlot after reopen = %v, %v", got, err)
	}
}

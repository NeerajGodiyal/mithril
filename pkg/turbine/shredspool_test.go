package turbine

import (
	"os"
	"path/filepath"
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

// The handoff contract: buffered slot-file tails and the completeness
// journal reach disk on Close, because the NEXT opener of the same directory
// (the block source after a prewarm handoff, or a turbine stream restart)
// sizes and reads it fresh and can only see flushed bytes. Writes after
// Close are dropped rather than silently reopening handles nobody closes.
func TestShredSpoolCloseFlushesBufferedTailsForNextOpener(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Append(700, []byte("buffered-tail"))
	first.MarkComplete(700, 3, 4)

	// Premise: the record is still in the bufio buffer — the on-disk file
	// exists but is empty, which is exactly what a second opener would
	// (wrongly) observe without the Close flush.
	info, err := os.Stat(filepath.Join(dir, "s700.shreds"))
	if err != nil {
		t.Fatalf("slot file should exist while buffered: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("premise broken: tail already flushed (size %d)", info.Size())
	}

	first.Close()
	first.Append(700, []byte("after-close"))

	second, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	got, err := second.ReadSlot(700)
	if err != nil || len(got) != 1 || string(got[0]) != "buffered-tail" {
		t.Fatalf("ReadSlot after handoff = %v, %v (want the buffered tail, no post-Close write)", got, err)
	}
	if meta, ok := second.IsComplete(700); !ok || meta.LastIndex != 3 || meta.Shreds != 4 {
		t.Fatalf("completeness must survive handoff (meta=%+v ok=%v)", meta, ok)
	}
}

// The completeness journal survives restarts and compacts away entries for
// deleted slot files — the index that lets a rebooted node know which slots
// need zero network, and the seed of repair-serving retention policy.
func TestShredSpoolCompletenessJournal(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first.Append(700, []byte("full"))
	first.Append(701, []byte("partial"))
	first.MarkComplete(700, 41, 42)
	first.Close()

	second, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	meta, ok := second.IsComplete(700)
	if !ok || meta.LastIndex != 41 || meta.Shreds != 42 {
		t.Fatalf("journal must survive reopen: %+v ok=%v", meta, ok)
	}
	if _, ok := second.IsComplete(701); ok {
		t.Fatalf("unmarked slot must not be complete")
	}
	// Deleting the slot (floor) drops the marker; the compacted journal on
	// the NEXT reopen must not resurrect it.
	second.SetFloor(701)
	if _, ok := second.IsComplete(700); ok {
		t.Fatalf("floor deletion must clear completeness")
	}
	second.Close()
	third, err := OpenShredSpool(dir, 0)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer third.Close()
	if _, ok := third.IsComplete(700); ok {
		t.Fatalf("stale journal entry must not resurrect a deleted slot")
	}
	if third.CompleteSlots() != 0 {
		t.Fatalf("no complete slots expected, got %d", third.CompleteSlots())
	}
}

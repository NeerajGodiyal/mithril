package turbine

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ShredSpool is a disposable on-disk cache of VERIFIED raw shreds, one
// append-only file per slot. It exists so catchup never has to hold either
// far-future decoded blocks or far-future shred state in RAM: shreds outside
// the assembly windows are appended here at receive time, and the hydrator
// reads whole slots back in batches as replay approaches. Repaired shreds
// are written too, so a slot reset re-hydrates from disk instead of
// re-fetching one shred per round trip.
//
// Durability is explicitly NOT a goal: no fsync, and a truncated tail record
// after a crash is skipped on read (the missing shreds re-repair). Files
// below the floor are deleted as replay advances; when the byte cap is
// exceeded the HIGHEST slots are dropped first — the live edge is cheap to
// re-fetch near the tip, the low end borders replay and is what repair
// would otherwise pay for dearly.
type ShredSpool struct {
	mu       sync.Mutex
	dir      string
	open     map[uint64]*spoolFile
	sizes    map[uint64]int64 // per-slot bytes on disk (open writers included)
	bytes    int64
	maxBytes int64
	floor    uint64
}

type spoolFile struct {
	f *os.File
	w *bufio.Writer
}

const (
	spoolOpenFilesCap = 32
	// Slots this close to the shred edge always assemble in RAM — broadcast
	// plus FEC recovery completes them essentially free.
	spoolLiveAssemblyLag = uint64(64)
)

// OpenShredSpool opens (or creates) a spool directory, adopting any slot
// files a previous process left behind — after a restart they hydrate
// instead of re-repairing.
func OpenShredSpool(dir string, maxBytes int64) (*ShredSpool, error) {
	if dir == "" {
		return nil, fmt.Errorf("shred spool needs a directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &ShredSpool{
		dir:      dir,
		open:     make(map[uint64]*spoolFile),
		sizes:    make(map[uint64]int64),
		maxBytes: maxBytes,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		slot, ok := spoolSlotFromName(e.Name())
		if !ok {
			continue
		}
		if info, err := e.Info(); err == nil {
			s.sizes[slot] = info.Size()
			s.bytes += info.Size()
		}
	}
	return s, nil
}

func spoolSlotFromName(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "s") || !strings.HasSuffix(name, ".shreds") {
		return 0, false
	}
	slot, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "s"), ".shreds"), 10, 64)
	return slot, err == nil
}

func (s *ShredSpool) pathFor(slot uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("s%d.shreds", slot))
}

// Append stores one verified shred packet (copied) under its slot.
func (s *ShredSpool) Append(slot uint64, packet []byte) {
	if len(packet) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot < s.floor {
		return
	}
	sf := s.open[slot]
	if sf == nil {
		if len(s.open) >= spoolOpenFilesCap {
			s.closeOldestLocked()
		}
		f, err := os.OpenFile(s.pathFor(slot), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return // disposable cache: failures degrade to repair
		}
		sf = &spoolFile{f: f, w: bufio.NewWriterSize(f, 64<<10)}
		s.open[slot] = sf
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(packet)))
	if _, err := sf.w.Write(hdr[:]); err != nil {
		return
	}
	if _, err := sf.w.Write(packet); err != nil {
		return
	}
	written := int64(4 + len(packet))
	s.sizes[slot] += written
	s.bytes += written
	s.enforceCapLocked()
}

// closeOldestLocked flushes and closes one open handle (lowest slot: it is
// the most likely to be hydrated soon, so its buffered tail must be on disk).
func (s *ShredSpool) closeOldestLocked() {
	var victim uint64
	first := true
	for slot := range s.open {
		if first || slot < victim {
			victim = slot
			first = false
		}
	}
	if !first {
		s.closeSlotLocked(victim)
	}
}

func (s *ShredSpool) closeSlotLocked(slot uint64) {
	if sf := s.open[slot]; sf != nil {
		_ = sf.w.Flush()
		_ = sf.f.Close()
		delete(s.open, slot)
	}
}

func (s *ShredSpool) enforceCapLocked() {
	if s.maxBytes <= 0 {
		return
	}
	for s.bytes > s.maxBytes {
		var victim uint64
		found := false
		for slot := range s.sizes {
			if !found || slot > victim {
				victim = slot
				found = true
			}
		}
		if !found {
			return
		}
		s.dropSlotLocked(victim)
	}
}

func (s *ShredSpool) dropSlotLocked(slot uint64) {
	s.closeSlotLocked(slot)
	s.bytes -= s.sizes[slot]
	delete(s.sizes, slot)
	_ = os.Remove(s.pathFor(slot))
}

// HasSlot reports whether any shreds are spooled for slot.
func (s *ShredSpool) HasSlot(slot uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sizes[slot] > 0
}

// SlotsInRange returns spooled slots within [lo, hi], ascending.
func (s *ShredSpool) SlotsInRange(lo, hi uint64) []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for slot := range s.sizes {
		if slot >= lo && slot <= hi {
			out = append(out, slot)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ReadSlot returns every spooled shred packet for slot. A truncated tail
// record (crash mid-append) is skipped silently.
func (s *ShredSpool) ReadSlot(slot uint64) ([][]byte, error) {
	s.mu.Lock()
	s.closeSlotLocked(slot) // flush any buffered tail
	path := s.pathFor(slot)
	s.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var packets [][]byte
	for off := 0; off+4 <= len(data); {
		n := int(binary.LittleEndian.Uint32(data[off : off+4]))
		off += 4
		if n <= 0 || off+n > len(data) {
			break
		}
		packets = append(packets, data[off:off+n])
		off += n
	}
	return packets, nil
}

// SetFloor advances the retention floor, deleting slot files strictly below
// it. Idempotent, monotonic.
func (s *ShredSpool) SetFloor(slot uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot <= s.floor {
		return
	}
	s.floor = slot
	for spooled := range s.sizes {
		if spooled < slot {
			s.dropSlotLocked(spooled)
		}
	}
}

// Stats reports spooled slot count and total bytes.
func (s *ShredSpool) Stats() (slots int, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sizes), s.bytes
}

// Close flushes and closes all open handles (files remain for the next
// process).
func (s *ShredSpool) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for slot := range s.open {
		s.closeSlotLocked(slot)
	}
}

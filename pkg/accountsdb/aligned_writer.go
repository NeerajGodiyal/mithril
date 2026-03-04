package accountsdb

import (
	"fmt"
	"os"
	"unsafe"
)

const (
	pageSize = 4096
)

// AlignedWriter writes sequentially to an O_DIRECT file descriptor.
// It accepts caller-provided page-aligned buffers and writes them directly
// to disk, avoiding an extra copy. A small internal carryover buffer handles
// the unaligned tail from each write so that all disk I/O is page-aligned.
//
// Typical usage:
//
//	buf := AlignedAlloc(size)          // page-aligned
//	copy(buf, data)
//	w.WriteAligned(buf[:dataLen])      // bulk goes straight to disk
//	// ... repeat ...
//	w.Flush()                          // flush trailing partial page
type AlignedWriter struct {
	f        *os.File
	carry    []byte // page-aligned, exactly one page
	carryLen int    // valid bytes in carry (0..pageSize-1)
	offset   int64  // next file write offset (always page-aligned)
}

// NewAlignedWriter creates an AlignedWriter for the given O_DIRECT file.
func NewAlignedWriter(f *os.File) *AlignedWriter {
	return &AlignedWriter{
		f:     f,
		carry: AlignedAlloc(pageSize),
	}
}

// WriteAligned writes p to disk. p MUST start at a page-aligned address
// (i.e. allocated via AlignedAlloc). The length does not need to be aligned.
//
// If there is carryover from a previous call, it is combined with the start
// of p into an aligned write. The bulk middle portion of p is written directly
// (zero-copy). Any trailing unaligned bytes are saved as carryover.
func (w *AlignedWriter) WriteAligned(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	// 1. If we have carryover, fill it to a full page and flush it.
	if w.carryLen > 0 {
		need := pageSize - w.carryLen
		if need > len(p) {
			// Not enough data to complete the page — just accumulate.
			copy(w.carry[w.carryLen:], p)
			w.carryLen += len(p)
			return nil
		}
		copy(w.carry[w.carryLen:], p[:need])
		w.carryLen = 0
		if err := w.writeAtAligned(w.carry[:pageSize], w.offset); err != nil {
			return err
		}
		w.offset += pageSize
		p = p[need:]
		if len(p) == 0 {
			return nil
		}
	}

	// 2. Write the bulk aligned middle directly from p (zero-copy).
	bulk := alignDown(len(p), pageSize)
	if bulk > 0 {
		if err := w.writeAtAligned(p[:bulk], w.offset); err != nil {
			return err
		}
		w.offset += int64(bulk)
		p = p[bulk:]
	}

	// 3. Stash any trailing unaligned bytes as carryover.
	if len(p) > 0 {
		copy(w.carry, p)
		w.carryLen = len(p)
	}

	return nil
}

// Write implements io.Writer but requires p to be page-aligned.
// Provided for compatibility; prefer WriteAligned for clarity.
func (w *AlignedWriter) Write(p []byte) (int, error) {
	err := w.WriteAligned(p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// TakeCarry copies any buffered carry bytes into dst and resets the
// internal carry state. Returns the number of bytes copied. This lets
// the caller place the carry at the start of a new page-aligned buffer
// so the next WriteAligned call starts from an aligned address.
func (w *AlignedWriter) TakeCarry(dst []byte) int {
	n := copy(dst, w.carry[:w.carryLen])
	w.carryLen = 0
	return n
}

// Flush writes any remaining carryover, padded to a page boundary.
func (w *AlignedWriter) Flush() error {
	if w.carryLen == 0 {
		return nil
	}
	// Zero-pad to page boundary
	for i := w.carryLen; i < pageSize; i++ {
		w.carry[i] = 0
	}
	if err := w.writeAtAligned(w.carry[:pageSize], w.offset); err != nil {
		return err
	}
	w.offset += pageSize
	w.carryLen = 0
	return nil
}

func (w *AlignedWriter) writeAtAligned(buf []byte, off int64) error {
	n, err := w.f.WriteAt(buf, off)
	if err != nil {
		return fmt.Errorf("AlignedWriter: WriteAt offset=%d len=%d: %w", off, len(buf), err)
	}
	if n != len(buf) {
		return fmt.Errorf("AlignedWriter: short write: %d != %d", n, len(buf))
	}
	return nil
}

func AlignUp(n, align int) int {
	return (n + align - 1) &^ (align - 1)
}

func alignDown(n, align int) int {
	return n &^ (align - 1)
}

// AlignedAlloc allocates a page-aligned byte slice of the given size.
func AlignedAlloc(size int) []byte {
	// Allocate extra bytes so we can find a page-aligned start within.
	raw := make([]byte, size+pageSize)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	offset := int(AlignUp(int(addr), pageSize) - int(addr))
	return raw[offset : offset+size]
}

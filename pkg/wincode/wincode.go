package wincode

import (
	"encoding/binary"
	"fmt"
	"math"
)

type Writer struct {
	buf []byte
}

func NewWriter(capacity int) *Writer {
	if capacity < 0 {
		capacity = 0
	}
	return &Writer{buf: make([]byte, 0, capacity)}
}

func (w *Writer) Bytes() []byte {
	return w.buf
}

func (w *Writer) WriteU16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}

func (w *Writer) WriteU32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

func (w *Writer) WriteU64(v uint64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}

func (w *Writer) WriteBytes(v []byte) {
	w.buf = append(w.buf, v...)
}

func (w *Writer) WriteFixedBytes(name string, v []byte, size int) error {
	if size < 0 {
		return fmt.Errorf("%s has invalid negative size %d", name, size)
	}
	if len(v) != size {
		return fmt.Errorf("%s has length %d, want %d", name, len(v), size)
	}
	w.WriteBytes(v)
	return nil
}

func (w *Writer) WriteByteVec(v []byte) {
	w.WriteU64(uint64(len(v)))
	w.WriteBytes(v)
}

type Reader struct {
	data []byte
	off  int
}

func NewReader(data []byte) *Reader {
	return &Reader{data: data}
}

func (r *Reader) Offset() int {
	return r.off
}

func (r *Reader) Remaining() int {
	return len(r.data) - r.off
}

func (r *Reader) EnsureEOF() error {
	if r.Remaining() != 0 {
		return fmt.Errorf("wincode: %d trailing bytes at offset %d", r.Remaining(), r.off)
	}
	return nil
}

func (r *Reader) ReadU16() (uint16, error) {
	if err := r.require(2); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint16(r.data[r.off : r.off+2])
	r.off += 2
	return v, nil
}

func (r *Reader) ReadU32() (uint32, error) {
	if err := r.require(4); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint32(r.data[r.off : r.off+4])
	r.off += 4
	return v, nil
}

func (r *Reader) ReadU64() (uint64, error) {
	if err := r.require(8); err != nil {
		return 0, err
	}
	v := binary.LittleEndian.Uint64(r.data[r.off : r.off+8])
	r.off += 8
	return v, nil
}

func (r *Reader) ReadBytes(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("wincode: negative byte count %d", n)
	}
	if err := r.require(n); err != nil {
		return nil, err
	}
	out := make([]byte, n)
	copy(out, r.data[r.off:r.off+n])
	r.off += n
	return out, nil
}

func (r *Reader) ReadByteVec(maxLen uint64) ([]byte, error) {
	n, err := r.ReadU64()
	if err != nil {
		return nil, err
	}
	if n > maxLen {
		return nil, fmt.Errorf("wincode: byte vector length %d exceeds limit %d", n, maxLen)
	}
	if n > uint64(math.MaxInt) {
		return nil, fmt.Errorf("wincode: byte vector length %d exceeds platform max int", n)
	}
	return r.ReadBytes(int(n))
}

func (r *Reader) require(n int) error {
	if n < 0 {
		return fmt.Errorf("wincode: negative byte count %d", n)
	}
	if r.Remaining() < n {
		return fmt.Errorf("wincode: need %d bytes at offset %d, have %d", n, r.off, r.Remaining())
	}
	return nil
}

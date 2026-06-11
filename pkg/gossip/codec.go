package gossip

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
)

type encoder struct {
	buf []byte
}

func (e *encoder) bytes() []byte {
	return e.buf
}

func (e *encoder) u8(v uint8) {
	e.buf = append(e.buf, v)
}

func (e *encoder) bool(v bool) {
	if v {
		e.u8(1)
		return
	}
	e.u8(0)
}

func (e *encoder) u16(v uint16) {
	e.buf = binary.LittleEndian.AppendUint16(e.buf, v)
}

func (e *encoder) u32(v uint32) {
	e.buf = binary.LittleEndian.AppendUint32(e.buf, v)
}

func (e *encoder) u64(v uint64) {
	e.buf = binary.LittleEndian.AppendUint64(e.buf, v)
}

func (e *encoder) variant(index uint32) {
	e.u32(index)
}

func (e *encoder) fixed(b []byte) {
	e.buf = append(e.buf, b...)
}

func (e *encoder) varint(v uint64) {
	for v >= 0x80 {
		e.u8(byte(v) | 0x80)
		v >>= 7
	}
	e.u8(byte(v))
}

func (e *encoder) shortLen(n int) error {
	if n > 0xffff {
		return fmt.Errorf("short_vec length %d overflows u16", n)
	}
	e.varint(uint64(n))
	return nil
}

type decoder struct {
	data []byte
	off  int
}

func newDecoder(data []byte) *decoder {
	return &decoder{data: data}
}

func (d *decoder) remaining() int {
	return len(d.data) - d.off
}

func (d *decoder) read(n int) ([]byte, error) {
	if n < 0 || d.remaining() < n {
		return nil, io.ErrUnexpectedEOF
	}
	out := d.data[d.off : d.off+n]
	d.off += n
	return out, nil
}

func (d *decoder) u8() (uint8, error) {
	b, err := d.read(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (d *decoder) bool() (bool, error) {
	v, err := d.u8()
	if err != nil {
		return false, err
	}
	switch v {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("invalid bool tag %d", v)
	}
}

func (d *decoder) u16() (uint16, error) {
	b, err := d.read(2)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b), nil
}

func (d *decoder) u32() (uint32, error) {
	b, err := d.read(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b), nil
}

func (d *decoder) u64() (uint64, error) {
	b, err := d.read(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(b), nil
}

func (d *decoder) variant() (uint32, error) {
	return d.u32()
}

func (d *decoder) varint(maxBytes int) (uint64, error) {
	var out uint64
	for i := 0; i < maxBytes; i++ {
		b, err := d.u8()
		if err != nil {
			return 0, err
		}
		out |= uint64(b&0x7f) << uint(7*i)
		if b&0x80 == 0 {
			return out, nil
		}
	}
	return 0, fmt.Errorf("varint overflows %d bytes", maxBytes)
}

func (d *decoder) shortLen() (int, error) {
	v, err := d.varint(3)
	if err != nil {
		return 0, err
	}
	if v > 0xffff {
		return 0, fmt.Errorf("short_vec length %d overflows u16", v)
	}
	return int(v), nil
}

func encodeIP(e *encoder, ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		e.variant(0)
		e.fixed(v4)
		return nil
	}
	v6 := ip.To16()
	if v6 == nil {
		return fmt.Errorf("invalid IP address %q", ip.String())
	}
	e.variant(1)
	e.fixed(v6)
	return nil
}

func decodeIP(d *decoder) (net.IP, error) {
	variant, err := d.variant()
	if err != nil {
		return nil, err
	}
	switch variant {
	case 0:
		b, err := d.read(4)
		if err != nil {
			return nil, err
		}
		return net.IPv4(b[0], b[1], b[2], b[3]), nil
	case 1:
		b, err := d.read(16)
		if err != nil {
			return nil, err
		}
		return net.IP(append([]byte(nil), b...)), nil
	default:
		return nil, fmt.Errorf("unknown IP variant %d", variant)
	}
}

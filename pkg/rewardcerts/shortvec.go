package rewardcerts

import "fmt"

func encodeShortU16(v uint16) []byte {
	if v < 0x80 {
		return []byte{byte(v)}
	}
	if v < 0x4000 {
		return []byte{byte(v&0x7f) | 0x80, byte(v >> 7)}
	}
	return []byte{
		byte(v&0x7f) | 0x80,
		byte((v>>7)&0x7f) | 0x80,
		byte(v >> 14),
	}
}

func decodeShortU16(data []byte) (uint16, int, error) {
	if len(data) == 0 {
		return 0, 0, fmt.Errorf("rewardcerts: empty short-u16")
	}
	if data[0]&0x80 == 0 {
		return uint16(data[0]), 1, nil
	}
	if len(data) < 2 {
		return 0, 0, fmt.Errorf("rewardcerts: truncated short-u16")
	}
	v := uint16(data[0]&0x7f) | uint16(data[1])<<7
	if data[1]&0x80 == 0 {
		return v, 2, nil
	}
	if len(data) < 3 {
		return 0, 0, fmt.Errorf("rewardcerts: truncated short-u16")
	}
	v |= uint16(data[2]) << 14
	return v, 3, nil
}

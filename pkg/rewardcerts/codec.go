package rewardcerts

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/wincode"
	"github.com/gagliardetto/solana-go"
)

type SkipRewardCertificate struct {
	Slot      uint64
	Signature [blsSignatureCompressedSize]byte
	Bitmap    []byte
}

type NotarRewardCertificate struct {
	Slot      uint64
	BlockID   solana.Hash
	Signature [blsSignatureCompressedSize]byte
	Bitmap    []byte
}

type RewardCertificates struct {
	Skip  []byte
	Notar []byte
}

func EncodeSkipRewardCertificate(cert SkipRewardCertificate) ([]byte, error) {
	if len(cert.Bitmap) > 0xffff {
		return nil, fmt.Errorf("rewardcerts: skip reward bitmap too large")
	}
	w := wincode.NewWriter(128 + len(cert.Bitmap))
	w.WriteU64(cert.Slot)
	if err := w.WriteFixedBytes("skip reward signature", cert.Signature[:], blsSignatureCompressedSize); err != nil {
		return nil, err
	}
	w.WriteBytes(encodeShortU16(uint16(len(cert.Bitmap))))
	w.WriteBytes(cert.Bitmap)
	return w.Bytes(), nil
}

func EncodeNotarRewardCertificate(cert NotarRewardCertificate) ([]byte, error) {
	if len(cert.Bitmap) > 0xffff {
		return nil, fmt.Errorf("rewardcerts: notar reward bitmap too large")
	}
	w := wincode.NewWriter(160 + len(cert.Bitmap))
	w.WriteU64(cert.Slot)
	w.WriteBytes(cert.BlockID[:])
	if err := w.WriteFixedBytes("notar reward signature", cert.Signature[:], blsSignatureCompressedSize); err != nil {
		return nil, err
	}
	w.WriteBytes(encodeShortU16(uint16(len(cert.Bitmap))))
	w.WriteBytes(cert.Bitmap)
	return w.Bytes(), nil
}

func SkipRewardCertificateEncodedLen(data []byte) (int, error) {
	if len(data) < 8+blsSignatureCompressedSize+1 {
		return 0, fmt.Errorf("rewardcerts: short skip reward cert")
	}
	bitmapLen, n, err := decodeShortU16(data[8+blsSignatureCompressedSize:])
	if err != nil {
		return 0, err
	}
	total := 8 + blsSignatureCompressedSize + n + int(bitmapLen)
	if total > len(data) {
		return 0, fmt.Errorf("rewardcerts: skip reward cert overflows buffer")
	}
	return total, nil
}

func NotarRewardCertificateEncodedLen(data []byte) (int, error) {
	if len(data) < 8+32+blsSignatureCompressedSize+1 {
		return 0, fmt.Errorf("rewardcerts: short notar reward cert")
	}
	bitmapLen, n, err := decodeShortU16(data[8+32+blsSignatureCompressedSize:])
	if err != nil {
		return 0, err
	}
	total := 8 + 32 + blsSignatureCompressedSize + n + int(bitmapLen)
	if total > len(data) {
		return 0, fmt.Errorf("rewardcerts: notar reward cert overflows buffer")
	}
	return total, nil
}

func DecodeSkipRewardCertificate(data []byte) (SkipRewardCertificate, error) {
	r := wincode.NewReader(data)
	slot, err := r.ReadU64()
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	sigRaw, err := r.ReadBytes(blsSignatureCompressedSize)
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	bitmapLen, n, err := decodeShortU16(data[r.Offset():])
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	bitmap, err := readBytesAt(data, r.Offset()+n, int(bitmapLen))
	if err != nil {
		return SkipRewardCertificate{}, err
	}
	if r.Offset()+n+int(bitmapLen) != len(data) {
		return SkipRewardCertificate{}, fmt.Errorf("rewardcerts: skip reward cert trailing bytes")
	}
	var cert SkipRewardCertificate
	cert.Slot = slot
	copy(cert.Signature[:], sigRaw)
	cert.Bitmap = bitmap
	return cert, nil
}

func DecodeNotarRewardCertificate(data []byte) (NotarRewardCertificate, error) {
	r := wincode.NewReader(data)
	slot, err := r.ReadU64()
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	blockID, err := r.ReadBytes(32)
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	sigRaw, err := r.ReadBytes(blsSignatureCompressedSize)
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	bitmapLen, n, err := decodeShortU16(data[r.Offset():])
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	bitmap, err := readBytesAt(data, r.Offset()+n, int(bitmapLen))
	if err != nil {
		return NotarRewardCertificate{}, err
	}
	if r.Offset()+n+int(bitmapLen) != len(data) {
		return NotarRewardCertificate{}, fmt.Errorf("rewardcerts: notar reward cert trailing bytes")
	}
	var cert NotarRewardCertificate
	cert.Slot = slot
	copy(cert.BlockID[:], blockID)
	copy(cert.Signature[:], sigRaw)
	cert.Bitmap = bitmap
	return cert, nil
}

func readBytesAt(data []byte, off, n int) ([]byte, error) {
	if off < 0 || n < 0 || off+n > len(data) {
		return nil, fmt.Errorf("rewardcerts: out of bounds read at %d len %d", off, n)
	}
	out := make([]byte, n)
	copy(out, data[off:off+n])
	return out, nil
}

package turbine

import (
	"encoding/binary"

	"github.com/gagliardetto/solana-go"
	"github.com/minio/sha256-simd"
)

// ShredID uniquely identifies a shred (slot, index, type).
type ShredID struct {
	Slot  uint64
	Index uint32
	Type  ShredType
}

// Seed returns the ChaCha RNG seed for turbine peer selection.
func (id ShredID) Seed(leader solana.PublicKey) [32]byte {
	h := sha256.New()
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], id.Slot)
	h.Write(buf[:])
	h.Write([]byte{id.Type.TurbineSeedTypeByte()})
	binary.LittleEndian.PutUint32(buf[:4], id.Index)
	h.Write(buf[:4])
	h.Write(leader[:])
	sum := h.Sum(nil)
	var out [32]byte
	copy(out[:], sum)
	return out
}

// ShredIDFromPacket parses the turbine ShredId fields from a shred packet.
func ShredIDFromPacket(packet []byte) (ShredID, error) {
	if len(packet) <= shredIndexOffset+4 {
		return ShredID{}, ErrShortShred
	}
	variant := packet[shredVariantOffset]
	shredType, err := classifyVariant(variant)
	if err != nil {
		return ShredID{}, err
	}
	return ShredID{
		Slot:  binary.LittleEndian.Uint64(packet[shredSlotOffset : shredSlotOffset+8]),
		Index: binary.LittleEndian.Uint32(packet[shredIndexOffset : shredIndexOffset+4]),
		Type:  shredType,
	}, nil
}

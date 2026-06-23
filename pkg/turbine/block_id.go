package turbine

import (
	"crypto/sha256"

	"github.com/gagliardetto/solana-go"
)

// ParentInfoLeaf is the double-merkle parent binding leaf for one FEC slice.
func ParentInfoLeaf(parentSlot uint64, parentBlockID solana.Hash, fecSetCount uint32) solana.Hash {
	buf := make([]byte, 8+32+4)
	copy(buf[0:8], uint64LE(parentSlot))
	copy(buf[8:40], parentBlockID[:])
	copy(buf[40:44], uint32LE(fecSetCount))
	return hashv([][]byte{buf})
}

// DoubleMerkleBlockID computes the Alpenglow block id from ordered FEC set roots.
func DoubleMerkleBlockID(parentSlot uint64, parentBlockID solana.Hash, fecSetRoots []solana.Hash) solana.Hash {
	if len(fecSetRoots) == 0 {
		return solana.Hash{}
	}
	fecSetCount := uint32(len(fecSetRoots))
	leaves := append([]solana.Hash(nil), fecSetRoots...)
	leaves = append(leaves, ParentInfoLeaf(parentSlot, parentBlockID, fecSetCount))
	return merkleRoot(leaves)
}

func joinDoubleMerkle(left, right solana.Hash) solana.Hash {
	return hashv([][]byte{
		[]byte("\x01SOLANA_MERKLE_SHREDS_NODE"),
		left[:20],
		right[:20],
	})
}

func merkleRoot(nodes []solana.Hash) solana.Hash {
	if len(nodes) == 0 {
		return solana.Hash{}
	}
	cur := append([]solana.Hash(nil), nodes...)
	for len(cur) > 1 {
		var next []solana.Hash
		for i := 0; i < len(cur); i += 2 {
			left := cur[i]
			right := left
			if i+1 < len(cur) {
				right = cur[i+1]
			}
			next = append(next, joinDoubleMerkle(left, right))
		}
		cur = next
	}
	return cur[0]
}

func hashv(parts [][]byte) solana.Hash {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write(part)
	}
	var out solana.Hash
	copy(out[:], h.Sum(nil))
	return out
}

func uint64LE(v uint64) []byte {
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(v >> (8 * i))
	}
	return buf[:]
}

func uint32LE(v uint32) []byte {
	var buf [4]byte
	for i := 0; i < 4; i++ {
		buf[i] = byte(v >> (8 * i))
	}
	return buf[:]
}

package bankhash

import "crypto/sha256"

const merkleFanout = 16

func divCeil(x uint64, y uint64) uint64 {
	result := x / y
	if (x % y) != 0 {
		result++
	}
	return result
}

func computeMerkleRootLoop(acctHashes [][]byte) []byte {
	if len(acctHashes) == 0 {
		return nil
	}

	totalHashes := uint64(len(acctHashes))
	chunks := divCeil(totalHashes, merkleFanout)

	results := make([][]byte, chunks)

	for i := uint64(0); i < chunks; i++ {
		startIdx := i * merkleFanout
		endIdx := min(startIdx+merkleFanout, totalHashes)

		hasher := sha256.New()
		a := acctHashes[startIdx:endIdx]

		for _, h := range a {
			hasher.Write(h)
		}

		results[i] = hasher.Sum(nil)
	}

	if len(results) == 1 {
		return results[0]
	} else {
		return computeMerkleRootLoop(results)
	}
}

package rewardcerts

import "fmt"

const (
	signerStoreVersionBase2 = byte(0)
	signerStoreHeaderLen    = 3
)

var errEmptyBitmap = fmt.Errorf("rewardcerts: empty signer bitmap")

func encodeSignerStoreBase2(bits []bool) ([]byte, error) {
	last := -1
	for i, set := range bits {
		if set {
			last = i
		}
	}
	if last < 0 {
		return nil, errEmptyBitmap
	}
	numBits := last + 1
	if numBits > 0xffff {
		return nil, fmt.Errorf("rewardcerts: bitmap length %d exceeds u16 max", numBits)
	}
	payload := make([]byte, (numBits+7)/8)
	for i := 0; i < numBits; i++ {
		if bits[i] {
			payload[i/8] |= 1 << uint(i%8)
		}
	}
	out := make([]byte, 0, signerStoreHeaderLen+len(payload))
	out = append(out, signerStoreVersionBase2, byte(numBits), byte(numBits>>8))
	out = append(out, payload...)
	return out, nil
}

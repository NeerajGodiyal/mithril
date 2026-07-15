package turbine

import "math/bits"

// uniformU64Sampler for solana's turbine rng
type uniformU64Sampler struct {
	rangeEnd uint64
	zone     uint64
}

func newUniformU64TraitSampler(rangeEnd uint64) uniformU64Sampler {
	if rangeEnd == 0 {
		return uniformU64Sampler{}
	}
	return uniformU64Sampler{
		rangeEnd: rangeEnd,
		zone:     (rangeEnd << bits.LeadingZeros64(rangeEnd)) - 1,
	}
}

func (s uniformU64Sampler) sample(rng rngSource) uint64 {
	if s.rangeEnd == 0 {
		return 0
	}
	for {
		hi, lo := wmul(rng.Uint64(), s.rangeEnd)
		if lo <= s.zone {
			return hi
		}
	}
}

func wmul(x, y uint64) (hi, lo uint64) {
	hi = (x >> 32) * (y >> 32)
	mid1 := (x >> 32) * (y & 0xffffffff)
	mid2 := (x & 0xffffffff) * (y >> 32)
	lo32 := (x & 0xffffffff) * (y & 0xffffffff)

	carry := (lo32 >> 32) + (mid1 & 0xffffffff) + (mid2 & 0xffffffff)
	hi += (mid1 >> 32) + (mid2 >> 32) + (carry >> 32)
	lo = (carry << 32) | (lo32 & 0xffffffff)
	return hi, lo
}

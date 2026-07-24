package accountsdb

import (
	"math/bits"
	"sync"

	"github.com/cespare/xxhash/v2"
	"github.com/gagliardetto/solana-go"
)

const (
	// Small batches are cheap enough to admit immediately. Large scans pass
	// through the two-hit doorkeeper below so one-shot block accounts cannot
	// churn the retained account cache.
	commonAccountImmediateAdmissionLimit = 5000

	// Bound cache writes from one large block. When more reusable candidates
	// are present, successive batches rotate through deterministic hash
	// partitions so a recurring working set is admitted progressively.
	commonAccountMaxAdmissionsPerBatch = 16 * 1024

	// Two 2 MiB Bloom generations retain roughly the previous 1-2 million
	// observations. Four probes keep the worst-window false-admission rate near
	// 2%; false positives affect cache performance only, never read correctness.
	commonAdmissionBloomWords       = (2 << 20) / 8
	commonAdmissionBloomProbes      = 4
	commonAdmissionRotateAfterKeys  = 1_000_000
	commonAdmissionSecondaryHashXor = uint64(0x9e3779b97f4a7c15)
)

// commonCacheAdmission is a bounded two-hit doorkeeper in front of Otter's
// S3-FIFO value cache. Classification is deliberately batch-atomic: every key
// is queried before any key from the same request is inserted, so duplicate
// pubkeys within one request cannot manufacture reusable heat.
type commonCacheAdmission struct {
	mu sync.Mutex

	current  []uint64
	previous []uint64
	observed uint64

	// partitionRound lets an oversized recurring working set fill the cache
	// over several blocks instead of selecting the same subset forever.
	partitionRound uint64
}

func newCommonCacheAdmission() *commonCacheAdmission {
	return &commonCacheAdmission{
		current:  make([]uint64, commonAdmissionBloomWords),
		previous: make([]uint64, commonAdmissionBloomWords),
	}
}

// classifyAndObserve marks which requested cold keys may be admitted after a
// successful account read. Small batches admit immediately. Large batches
// require an observation from an earlier batch and cap admissions by rotating
// through deterministic hash partitions.
func (a *commonCacheAdmission) classifyAndObserve(
	pks []solana.PublicKey,
	cold []int,
) []bool {
	admit := make([]bool, len(pks))
	if len(cold) == 0 {
		return admit
	}
	if a == nil {
		if len(cold) <= commonAccountImmediateAdmissionLimit {
			for _, idx := range cold {
				admit[idx] = true
			}
		}
		return admit
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(cold) <= commonAccountImmediateAdmissionLimit {
		for _, idx := range cold {
			admit[idx] = true
		}
		a.rotateBeforeInsert(uint64(len(cold)))
		for _, idx := range cold {
			a.insert(pks[idx])
		}
		a.observed += uint64(len(cold))
		return admit
	}

	// Query the complete batch before inserting any current-batch key.
	candidateCount := 0
	for _, idx := range cold {
		if a.contains(pks[idx]) {
			admit[idx] = true
			candidateCount++
		}
	}

	shift := admissionPartitionShift(candidateCount)
	var partitionMask uint64
	var selectedPartition uint64
	if shift > 0 {
		partitionMask = (uint64(1) << shift) - 1
		selectedPartition = a.partitionRound & partitionMask
		a.partitionRound++
	}
	admissionCount := 0
	for _, idx := range cold {
		if !admit[idx] {
			continue
		}
		if admissionCount < commonAccountMaxAdmissionsPerBatch &&
			(shift == 0 || admissionPartitionHash(pks[idx])&partitionMask == selectedPartition) {
			admissionCount++
		} else {
			admit[idx] = false
		}
	}

	a.rotateBeforeInsert(uint64(len(cold)))
	for _, idx := range cold {
		a.insert(pks[idx])
	}
	a.observed += uint64(len(cold))
	return admit
}

func admissionPartitionShift(candidateCount int) uint {
	if candidateCount <= commonAccountMaxAdmissionsPerBatch {
		return 0
	}
	ratio := (candidateCount + commonAccountMaxAdmissionsPerBatch - 1) /
		commonAccountMaxAdmissionsPerBatch
	return uint(bits.Len(uint(ratio - 1)))
}

func (a *commonCacheAdmission) rotateBeforeInsert(incoming uint64) {
	if a.observed == 0 || a.observed+incoming <= commonAdmissionRotateAfterKeys {
		return
	}
	a.previous, a.current = a.current, a.previous
	clear(a.current)
	a.observed = 0
}

func (a *commonCacheAdmission) contains(pk solana.PublicKey) bool {
	h1, h2 := commonAdmissionHashes(pk)
	mask := uint64(len(a.current)*64 - 1)
	for i := uint64(0); i < commonAdmissionBloomProbes; i++ {
		bit := (h1 + i*h2) & mask
		word, bitInWord := bit>>6, bit&63
		flag := uint64(1) << bitInWord
		if a.current[word]&flag == 0 && a.previous[word]&flag == 0 {
			return false
		}
	}
	return true
}

func (a *commonCacheAdmission) insert(pk solana.PublicKey) {
	h1, h2 := commonAdmissionHashes(pk)
	mask := uint64(len(a.current)*64 - 1)
	for i := uint64(0); i < commonAdmissionBloomProbes; i++ {
		bit := (h1 + i*h2) & mask
		a.current[bit>>6] |= uint64(1) << (bit & 63)
	}
}

func commonAdmissionHashes(pk solana.PublicKey) (uint64, uint64) {
	h1 := xxhash.Sum64(pk[:])
	h2 := mixAdmissionHash(h1^commonAdmissionSecondaryHashXor) | 1
	return h1, h2
}

func admissionPartitionHash(pk solana.PublicKey) uint64 {
	h1, h2 := commonAdmissionHashes(pk)
	return mixAdmissionHash(h1 + h2)
}

func mixAdmissionHash(x uint64) uint64 {
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}

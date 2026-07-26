package sigverify

// MaxDrain is how many work items a verification worker will coalesce into one
// batch.
//
// The accelerated backend verifies eight signatures per AVX-512 group, so any
// multiple of eight keeps every lane busy. 64 is eight full groups: large
// enough that per-batch overhead is negligible, small enough that one worker
// cannot monopolise a shared queue while its peers idle, and small enough that
// the scratch a worker holds stays in cache.
const MaxDrain = 64

// Drain coalesces work from ch into dst, starting with an item the caller has
// already received.
//
// The shape matters and is the whole point of this helper. The caller blocks
// receiving the first item — a worker with nothing to do should sleep, not
// spin — and everything after that is a non-blocking peek. So Drain NEVER waits
// for a batch to fill. There is no timer and no minimum size.
//
// That is what makes batching safe to bolt onto latency-sensitive paths: when
// work is scarce, a batch is whatever happened to be queued (often one item)
// and latency is unchanged; when work is abundant, batches fill naturally and
// the accelerated path pays off. A batching timer would trade the first case
// away to improve the second, and the first case is the tip of the chain.
//
// dst is reused across calls; pass the same worker-local slice every time and
// Drain will not allocate after it has grown once.
func Drain[T any](dst []T, first T, ch <-chan T, max int) []T {
	if max < 1 {
		max = 1
	}
	dst = append(dst[:0], first)
	for len(dst) < max {
		select {
		case item, open := <-ch:
			if !open {
				return dst
			}
			dst = append(dst, item)
		default:
			return dst
		}
	}
	return dst
}

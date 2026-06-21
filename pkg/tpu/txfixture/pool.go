package txfixture

// Pool is a precomputed set of distinct signed transfer wire transactions.
type Pool struct {
	wire [][]byte
}

// NewPool precomputes signed transfer transactions for bench and test fixtures.
func NewPool(n int) *Pool {
	if n <= 0 {
		return &Pool{}
	}
	return &Pool{wire: PrecomputeTransferPool(n)}
}

// Wire returns wire bytes for seq, cycling through the precomputed pool.
func (p *Pool) Wire(seq uint64) []byte {
	if len(p.wire) == 0 {
		return MustSignedTransferWire(seq)
	}
	return p.wire[seq%uint64(len(p.wire))]
}

// Len returns the number of precomputed wire transactions.
func (p *Pool) Len() int {
	return len(p.wire)
}

// Slice returns the underlying precomputed wire transactions.
func (p *Pool) Slice() [][]byte {
	return p.wire
}

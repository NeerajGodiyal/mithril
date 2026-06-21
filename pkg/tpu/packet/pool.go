package packet

// Pool provides fixed-size reusable transaction buffers.
type Pool struct {
	bufs [][DataSize]byte
	free chan uint32
}

func NewPool(slots int) *Pool {
	pool := &Pool{
		bufs: make([][DataSize]byte, slots),
		free: make(chan uint32, slots),
	}
	for i := range pool.bufs {
		pool.free <- uint32(i)
	}
	return pool
}

func (p *Pool) Acquire() ([]byte, uint32, bool) {
	select {
	case idx := <-p.free:
		return p.bufs[idx][:], idx, true
	default:
		return nil, 0, false
	}
}

func (p *Pool) Release(idx uint32) {
	p.free <- idx
}

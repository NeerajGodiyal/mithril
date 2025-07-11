package sbpf

import (
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/sbpf/sbpfver"
)

// Stack is the VM's call frame stack.
//
// # Memory stack
//
// The memory stack resides in addressable memory at VaddrStack.
//
// It is split into statically sized stack frames (StackFrameSize).
// Each frame stores spilled function arguments and local variables.
// The frame pointer (r10) points to the highest address in the current frame.
//
// New frames get allocated upwards.
// Each frame is followed by a gap of size StackFrameSize.
//
//	[0x1_0000_0000]: Frame
//	[0x1_0000_1000]: Gap
//	[0x1_0000_2000]: Frame
//	[0x1_0000_3000]: Gap
//	...
//
// # Shadow stack
//
// The shadow stack is not directly accessible from SBF.
// It stores return addresses and caller-preserved registers.
type Stack struct {
	mem                []byte
	sp                 uint64
	shadow             []Frame
	dynamicStackFrames bool
}

// Frame is an entry on the shadow stack.
type Frame struct {
	FramePtr uint64
	NVRegs   [4]uint64
	RetAddr  int64
}

// StackFrameSize is the addressable memory within a stack frame.
//
// Note that this constant cannot be changed trivially.
const (
	StackFrameSize          = 0x1000
	StackMax                = 64 * StackFrameSize
	DynamicStackFramesAlign = 64
)

// StackDepth is the max frame count of the stack.
const StackDepth = 64

func newStackMem() []byte {
	return make([]byte, StackMax)
}

func newStackShadow() []Frame {
	return make([]Frame, 1, StackDepth)
}

var (
	// Also applies to interpreter heap.
	UsePool = true

	stackMemPool = &sync.Pool{New: func() interface{} {
		return newStackMem()
	}}

	stackShadowPool = &sync.Pool{New: func() interface{} {
		return newStackShadow()
	}}
)

func NewStack(sbpfVer sbpfver.SbpfVersion) Stack {
	var m []byte
	var sh []Frame
	if UsePool {
		m = stackMemPool.Get().([]byte)
		sh = stackShadowPool.Get().([]Frame)
	} else {
		m = newStackMem()
		sh = newStackShadow()
	}

	s := Stack{
		mem:    m,
		sp:     VaddrStack,
		shadow: sh,
	}

	var sz uint64
	if sbpfVer.DynamicStackFrames() {
		sz = StackMax
		s.dynamicStackFrames = true
	} else {
		sz = StackFrameSize
	}

	s.shadow[0] = Frame{
		FramePtr: VaddrStack + sz,
	}
	return s
}

func (s *Stack) Finish() {
	if UsePool {
		go func() {
			s.mem = s.mem[:StackDepth*StackFrameSize]
			clear(s.mem)
			stackMemPool.Put(s.mem)
			s.shadow = s.shadow[:StackDepth]
			clear(s.shadow)
			s.shadow = s.shadow[:1]
			stackShadowPool.Put(s.shadow)
		}()
	}
}

// GetFramePtr returns the current frame pointer.
func (s *Stack) GetFramePtr() uint64 {
	return s.shadow[len(s.shadow)-1].FramePtr
}

// GetFrame returns the stack frame memory slice containing the frame pointer.
//
// The returned slice starts at the location within the frame as indicated by the address.
// To get the full frame, align the provided address by StackFrameSize.
//
// Returns nil if the program tries to address a gap or out-of-bounds memory.
func (s *Stack) GetFrame(addr uint32) []byte {
	hi, lo := addr/StackFrameSize, addr%StackFrameSize
	if hi%2 == 1 && !s.dynamicStackFrames {
		return nil
	}

	pos := hi / 2
	off := pos * StackFrameSize

	return s.mem[off+lo: /*off+StackFrameSize*/]
}

// Push allocates a new call frame.
//
// Saves the given nonvolatile regs and return address.
// Returns the new frame pointer.
func (s *Stack) Push(nvRegs *[4]uint64, ret int64) (fp uint64, ok bool) {
	if ok = len(s.shadow) < cap(s.shadow); !ok {
		return
	}

	if !s.dynamicStackFrames {
		fp = s.GetFramePtr() + 2*StackFrameSize
	} else {
		fp = s.GetFramePtr()
	}

	s.shadow = s.shadow[:len(s.shadow)+1]
	s.shadow[len(s.shadow)-1] = Frame{
		FramePtr: fp,
		NVRegs:   *nvRegs,
		RetAddr:  ret,
	}
	s.sp = fp - StackFrameSize
	return
}

// Pop exits the last call frame.
//
// Writes saved nonvolatile regs into provided slice.
// Returns saved return address, new frame pointer.
// Sets `ok` to false if no call frames are left.
func (s *Stack) Pop(nvRegs *[4]uint64) (fp uint64, ret int64, ok bool) {
	if len(s.shadow) <= 1 {
		ok = false
		return
	}

	var frame Frame
	frame, s.shadow = s.shadow[len(s.shadow)-1], s.shadow[:len(s.shadow)-1]

	fp = s.GetFramePtr()
	*nvRegs = frame.NVRegs
	ret = frame.RetAddr
	ok = true
	return
}

package wire

import (
	"errors"
	"fmt"

	bin "github.com/gagliardetto/binary"
)

const PacketDataSize = 1232

var (
	ErrEmpty            = errors.New("empty transaction")
	ErrTooLarge         = errors.New("transaction exceeds packet size")
	ErrInvalidEncoding  = errors.New("invalid compact-u16 encoding")
	ErrInvalidSigCount  = errors.New("invalid signature count")
	ErrInvalidMessage   = errors.New("invalid message encoding")
	ErrSigCountMismatch = errors.New("signature count mismatch")
	ErrInsufficientData = errors.New("insufficient transaction data")
)

// View is a zero-copy parsed legacy transaction wire layout.
type View struct {
	Wire          []byte
	NumSignatures int
	SigsOffset    int
	MessageOffset int
}

// ParseLegacy parses a legacy (non-versioned) transaction without copying wire bytes.
func ParseLegacy(wire []byte) (View, error) {
	if len(wire) == 0 {
		return View{}, ErrEmpty
	}
	if len(wire) > PacketDataSize {
		return View{}, ErrTooLarge
	}

	numSigs, size, err := bin.DecodeCompactU16(wire)
	if err != nil {
		return View{}, fmt.Errorf("%w: %v", ErrInvalidEncoding, err)
	}
	if numSigs <= 0 || numSigs > 255 {
		return View{}, ErrInvalidSigCount
	}

	sigsOffset := size
	sigsLen := numSigs * 64
	if len(wire) < sigsOffset+sigsLen {
		return View{}, ErrInsufficientData
	}

	msgOffset := sigsOffset + sigsLen
	if msgOffset >= len(wire) {
		return View{}, ErrInsufficientData
	}
	if wire[msgOffset] >= 0x80 {
		// cavey TODO(v0-tpu): accept versioned (v0) transactions
		return View{}, ErrInvalidMessage
	}

	return View{
		Wire:          wire,
		NumSignatures: numSigs,
		SigsOffset:    sigsOffset,
		MessageOffset: msgOffset,
	}, nil
}

func (v View) FirstSignature() []byte {
	if v.NumSignatures == 0 {
		return nil
	}
	return v.Wire[v.SigsOffset : v.SigsOffset+64]
}

func (v View) Message() []byte {
	return v.Wire[v.MessageOffset:]
}

// Sanitize performs pre-sigverify structural checks on a legacy transaction.
func Sanitize(wire []byte) (View, error) {
	v, err := ParseLegacy(wire)
	if err != nil {
		return View{}, err
	}

	msg := v.Message()
	if len(msg) < 4 {
		return View{}, ErrInsufficientData
	}

	required := int(msg[0])
	if required <= 0 || required != v.NumSignatures {
		return View{}, ErrSigCountMismatch
	}
	if msg[1] >= msg[0] {
		return View{}, ErrInvalidMessage
	}

	pos := 3
	numAccounts, n, err := bin.DecodeCompactU16(msg[pos:])
	if err != nil {
		return View{}, fmt.Errorf("%w: account keys: %v", ErrInvalidEncoding, err)
	}
	pos += n
	if numAccounts < required || numAccounts > 256 {
		return View{}, ErrInvalidMessage
	}
	if len(msg) < pos+numAccounts*32+32 {
		return View{}, ErrInsufficientData
	}
	pos += numAccounts * 32
	pos += 32 // recent blockhash

	numInstructions, n, err := bin.DecodeCompactU16(msg[pos:])
	if err != nil {
		return View{}, fmt.Errorf("%w: instructions: %v", ErrInvalidEncoding, err)
	}
	pos += n

	for i := 0; i < numInstructions; i++ {
		if len(msg) < pos+1 {
			return View{}, ErrInsufficientData
		}
		pos++

		numAccounts, n, err := bin.DecodeCompactU16(msg[pos:])
		if err != nil {
			return View{}, fmt.Errorf("%w: ix %d accounts: %v", ErrInvalidEncoding, i, err)
		}
		pos += n
		if len(msg) < pos+int(numAccounts) {
			return View{}, ErrInsufficientData
		}
		pos += int(numAccounts)

		dataLen, n, err := bin.DecodeCompactU16(msg[pos:])
		if err != nil {
			return View{}, fmt.Errorf("%w: ix %d data: %v", ErrInvalidEncoding, i, err)
		}
		pos += n
		if len(msg) < pos+dataLen {
			return View{}, ErrInsufficientData
		}
		pos += dataLen
	}

	if pos != len(msg) {
		return View{}, ErrInvalidMessage
	}
	return v, nil
}

package wire

import (
	"errors"
	"fmt"

	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

const (
	PacketDataSize   = 1232
	V1PacketDataSize = solana.MaxTransactionSizeV1
)

var (
	ErrEmpty            = errors.New("empty transaction")
	ErrTooLarge         = errors.New("transaction exceeds packet size")
	ErrInvalidEncoding  = errors.New("invalid compact-u16 encoding")
	ErrInvalidSigCount  = errors.New("invalid signature count")
	ErrInvalidMessage   = errors.New("invalid message encoding")
	ErrSigCountMismatch = errors.New("signature count mismatch")
	ErrInsufficientData = errors.New("insufficient transaction data")
)

// View is a zero-copy parsed transaction wire layout.
type View struct {
	Wire          []byte
	NumSignatures int
	SigsOffset    int
	MessageOffset int
	MessageLen    int
}

// ParseLegacy preserves the original API name but accepts both legacy and v0
// messages; callers only need signature/message offsets.
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
	return View{
		Wire:          wire,
		NumSignatures: numSigs,
		SigsOffset:    sigsOffset,
		MessageOffset: msgOffset,
		MessageLen:    len(wire) - msgOffset,
	}, nil
}

func (v View) FirstSignature() []byte {
	if v.NumSignatures == 0 {
		return nil
	}
	return v.Wire[v.SigsOffset : v.SigsOffset+64]
}

func (v View) Message() []byte {
	return v.Wire[v.MessageOffset : v.MessageOffset+v.MessageLen]
}

// Sanitize performs bounded pre-sigverify structural checks on all supported transactions.
func Sanitize(wire []byte) (View, error) {
	if len(wire) > 0 && wire[0] == 0x81 {
		return sanitizeV1(wire)
	}
	v, err := ParseLegacy(wire)
	if err != nil {
		return View{}, err
	}

	msg := v.Message()
	if len(msg) < 4 {
		return View{}, ErrInsufficientData
	}

	pos := 0
	versioned := msg[0]&0x80 != 0
	if versioned {
		if msg[0] != 0x80 {
			return View{}, ErrInvalidMessage
		}
		pos++
	}
	if len(msg) < pos+3 {
		return View{}, ErrInsufficientData
	}
	required := int(msg[pos])
	if required <= 0 || required != v.NumSignatures {
		return View{}, ErrSigCountMismatch
	}
	if msg[pos+1] >= msg[pos] {
		return View{}, ErrInvalidMessage
	}

	pos += 3
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
	if versioned {
		numLookups, n, err := bin.DecodeCompactU16(msg[pos:])
		if err != nil {
			return View{}, fmt.Errorf("%w: lookups: %v", ErrInvalidEncoding, err)
		}
		pos += n
		dynamicKeys := 0
		for i := 0; i < numLookups; i++ {
			if len(msg) < pos+32 {
				return View{}, ErrInsufficientData
			}
			pos += 32
			for j := 0; j < 2; j++ {
				count, n, err := bin.DecodeCompactU16(msg[pos:])
				if err != nil {
					return View{}, fmt.Errorf("%w: lookup %d indexes: %v", ErrInvalidEncoding, i, err)
				}
				pos += n
				if len(msg) < pos+count {
					return View{}, ErrInsufficientData
				}
				pos += count
				dynamicKeys += count
			}
		}
		if numAccounts+dynamicKeys > 256 {
			return View{}, ErrInvalidMessage
		}
	}

	if pos != len(msg) {
		return View{}, ErrInvalidMessage
	}
	return v, nil
}

func sanitizeV1(wire []byte) (View, error) {
	if len(wire) > V1PacketDataSize {
		return View{}, ErrTooLarge
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil || tx.Message.GetVersion() != solana.MessageVersionV1 {
		return View{}, ErrInvalidMessage
	}
	if err := tx.Sanitize(); err != nil {
		return View{}, ErrInvalidMessage
	}
	numSignatures := len(tx.Signatures)
	messageLen := len(wire) - numSignatures*solana.SignatureLength
	if messageLen <= 0 {
		return View{}, ErrInsufficientData
	}
	return View{
		Wire:          wire,
		NumSignatures: numSignatures,
		SigsOffset:    messageLen,
		MessageOffset: 0,
		MessageLen:    messageLen,
	}, nil
}

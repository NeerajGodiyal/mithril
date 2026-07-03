package turbine

import (
	"encoding/binary"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// FinalCertificate wincode layout: slot(u64 LE) | block_id(32) | final_aggregate |
// notar Option. VotesAggregate = signature([96] compressed G2) | bitmap(u16 LE len).
const blsCompressedSigLen = 96

// votesAggregateEncodedLen returns the byte length of a VotesAggregate at data[0:].
func votesAggregateEncodedLen(data []byte) (int, error) {
	if len(data) < blsCompressedSigLen+2 {
		return 0, fmt.Errorf("%w: truncated votes aggregate", ErrInvalidBlockComponent)
	}
	bitmapLen := int(binary.LittleEndian.Uint16(data[blsCompressedSigLen : blsCompressedSigLen+2]))
	total := blsCompressedSigLen + 2 + bitmapLen
	if total > len(data) {
		return 0, fmt.Errorf("%w: votes aggregate bitmap overflow", ErrInvalidBlockComponent)
	}
	return total, nil
}

// FinalCertificateEncodedLen returns the exact wincode-encoded length of a
// FinalCertificate at data[0:], so the enclosing footer option stops at its true
// end rather than swallowing the trailing reward certs.
func FinalCertificateEncodedLen(data []byte) (int, error) {
	pos := 8 + 32 // slot + block_id
	if pos > len(data) {
		return 0, fmt.Errorf("%w: truncated final cert header", ErrInvalidBlockComponent)
	}
	finalLen, err := votesAggregateEncodedLen(data[pos:])
	if err != nil {
		return 0, err
	}
	pos += finalLen
	if pos >= len(data) {
		return 0, fmt.Errorf("%w: missing notar-aggregate option tag", ErrInvalidBlockComponent)
	}
	tag := data[pos]
	pos++
	switch tag {
	case 0: // None
	case 1: // Some(VotesAggregate)
		notarLen, err := votesAggregateEncodedLen(data[pos:])
		if err != nil {
			return 0, err
		}
		pos += notarLen
	default:
		return 0, fmt.Errorf("%w: invalid notar-aggregate option tag %d", ErrInvalidBlockComponent, tag)
	}
	return pos, nil
}

func decodeVotesAggregate(data []byte) (VotesAggregate, int, error) {
	n, err := votesAggregateEncodedLen(data)
	if err != nil {
		return VotesAggregate{}, 0, err
	}
	var agg VotesAggregate
	copy(agg.Signature[:], data[:blsCompressedSigLen])
	bitmap := data[blsCompressedSigLen+2 : n]
	agg.Bitmap = append([]byte(nil), bitmap...)
	return agg, n, nil
}

// UnmarshalFinalCertificate decodes the raw footer final_cert bytes.
func UnmarshalFinalCertificate(data []byte) (*BlockFinalizationCert, error) {
	if len(data) < 8+32 {
		return nil, fmt.Errorf("%w: truncated final cert", ErrInvalidBlockComponent)
	}
	cert := &BlockFinalizationCert{}
	cert.Slot = binary.LittleEndian.Uint64(data[0:8])
	cert.BlockID = solana.HashFromBytes(data[8:40])
	pos := 40

	final, n, err := decodeVotesAggregate(data[pos:])
	if err != nil {
		return nil, err
	}
	cert.FinalAggregate = final
	pos += n

	if pos >= len(data) {
		return nil, fmt.Errorf("%w: missing notar option", ErrInvalidBlockComponent)
	}
	tag := data[pos]
	pos++
	if tag == 1 {
		notar, _, err := decodeVotesAggregate(data[pos:])
		if err != nil {
			return nil, err
		}
		cert.NotarAggregate = &notar
	} else if tag != 0 {
		return nil, fmt.Errorf("%w: invalid notar option tag %d", ErrInvalidBlockComponent, tag)
	}
	return cert, nil
}

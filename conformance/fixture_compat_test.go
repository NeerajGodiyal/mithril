package conformance

import (
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Firedancer's fixture schema gained a metadata message at field 1 and pushed
// input and output to fields 2 and 3. The generated bindings in this package
// still describe the older shape, where input is field 1 and output is field 2,
// so proto.Unmarshal on a current fixture reads the metadata submessage as an
// InstrContext. Protobuf is permissive about that: it does not error, it
// produces a context with no program id and no accounts, and every test built
// on it silently compares nothing.
//
// Only the outer wrapper drifted. InstrContext and InstrEffects still match, so
// rather than regenerate the descriptors (which needs protoc and a protosol
// checkout) this splits the wrapper by hand and unmarshals the two submessages
// with the existing types.
//
// The proper fix is to regenerate from firedancer-io/protosol v5.3.0, the
// version solana-conformance pins. Until then this keeps the corpus usable and
// fails loudly on a shape it does not recognise instead of quietly passing.
const (
	fixtureFieldMetadata = 1
	fixtureFieldInput    = 2
	fixtureFieldOutput   = 3
)

// unmarshalInstrFixture decodes a fixture under the current upstream schema.
func unmarshalInstrFixture(raw []byte) (*InstrFixture, error) {
	parts, err := splitTopLevelMessage(raw)
	if err != nil {
		return nil, err
	}

	// A fixture written against the old schema has no metadata field. Refuse it
	// rather than guessing, so a corpus mismatch cannot look like a pass.
	if _, ok := parts[fixtureFieldMetadata]; !ok {
		return nil, fmt.Errorf("fixture has no metadata field; corpus predates the schema this shim targets")
	}
	inputBytes, ok := parts[fixtureFieldInput]
	if !ok {
		return nil, fmt.Errorf("fixture has no input field")
	}

	fixture := &InstrFixture{Input: &InstrContext{}, Output: &InstrEffects{}}
	if err := proto.Unmarshal(inputBytes, fixture.Input); err != nil {
		return nil, fmt.Errorf("decode input: %w", err)
	}

	if outputBytes, ok := parts[fixtureFieldOutput]; ok {
		if err := proto.Unmarshal(outputBytes, fixture.Output); err != nil {
			return nil, fmt.Errorf("decode output: %w", err)
		}
	}
	return fixture, nil
}

// splitTopLevelMessage returns the raw bytes of each length-delimited field in
// a protobuf message, keyed by field number. Non-length-delimited fields are
// skipped: the fixture wrapper has none, and reading past one would desync the
// whole parse.
func splitTopLevelMessage(raw []byte) (map[int][]byte, error) {
	parts := make(map[int][]byte)
	for offset := 0; offset < len(raw); {
		key, n := binary.Uvarint(raw[offset:])
		if n <= 0 {
			return nil, fmt.Errorf("malformed field key at byte %d", offset)
		}
		offset += n

		field, wireType := int(key>>3), int(key&7)
		switch wireType {
		case 0: // varint
			_, n := binary.Uvarint(raw[offset:])
			if n <= 0 {
				return nil, fmt.Errorf("malformed varint for field %d", field)
			}
			offset += n
		case 1: // 64-bit
			offset += 8
		case 2: // length-delimited
			length, n := binary.Uvarint(raw[offset:])
			if n <= 0 {
				return nil, fmt.Errorf("malformed length for field %d", field)
			}
			offset += n
			end := offset + int(length)
			if end > len(raw) || end < offset {
				return nil, fmt.Errorf("field %d length %d overruns the message", field, length)
			}
			parts[field] = raw[offset:end]
			offset = end
		case 5: // 32-bit
			offset += 4
		default:
			return nil, fmt.Errorf("unsupported wire type %d for field %d", wireType, field)
		}
		if offset > len(raw) {
			return nil, fmt.Errorf("field %d overruns the message", field)
		}
	}
	return parts, nil
}

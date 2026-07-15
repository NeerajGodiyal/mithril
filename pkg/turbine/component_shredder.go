package turbine

import (
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// Shredder builds merkle FEC shreds from Alpenglow block components.
type Shredder struct {
	Slot          uint64
	ParentSlot    uint64
	Version       uint16
	ReferenceTick uint8
}

// ShredBatch is one FEC batch emitted for a single block component.
type ShredBatch struct {
	Slot              uint64
	Component         BlockComponent
	DataShreds        []*Shred
	CodeShreds        []*Shred
	Packets           [][]byte
	ChainedMerkleRoot solana.Hash
	IsLastInSlot      bool
}

// MakeMerkleShredsFromComponent serializes and shreds one block component.
func (s *Shredder) MakeMerkleShredsFromComponent(
	leader solana.PrivateKey,
	component BlockComponent,
	isLastInSlot bool,
	chainedMerkleRoot solana.Hash,
	nextShredIndex uint32,
	nextCodeIndex uint32,
) (ShredBatch, uint32, uint32, error) {
	bytes, err := MarshalBlockComponent(component)
	if err != nil {
		return ShredBatch{}, nextShredIndex, nextCodeIndex, err
	}
	gen := ShredGenerator{
		Slot:          s.Slot,
		ParentSlot:    s.ParentSlot,
		Version:       s.Version,
		ReferenceTick: s.ReferenceTick,
	}
	packets, root, nextData, nextCode, err := gen.MakeShredsFromData(
		leader,
		bytes,
		isLastInSlot,
		chainedMerkleRoot,
		nextShredIndex,
		nextCodeIndex,
	)
	if err != nil {
		return ShredBatch{}, nextShredIndex, nextCodeIndex, err
	}
	batch := ShredBatch{
		Slot:              s.Slot,
		Component:         component,
		Packets:           packets,
		ChainedMerkleRoot: root,
		IsLastInSlot:      isLastInSlot,
	}
	for _, packet := range packets {
		shred, err := ParseShred(packet)
		if err != nil {
			return ShredBatch{}, nextShredIndex, nextCodeIndex, fmt.Errorf("parse generated shred: %w", err)
		}
		if shred.Type == ShredTypeData {
			batch.DataShreds = append(batch.DataShreds, shred)
		} else {
			batch.CodeShreds = append(batch.CodeShreds, shred)
		}
	}
	return batch, nextData, nextCode, nil
}

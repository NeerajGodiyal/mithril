package turbine

import "github.com/gagliardetto/solana-go"

// BlockComponent is the Alpenglow broadcast unit shredded into FEC batches.
// It is either a batch of ledger entries or a block metadata marker.
type BlockComponent struct {
	EntryBatch []Entry
	Marker     *BlockMarker
}

// BlockMarker carries Alpenglow block metadata outside the entry stream.
type BlockMarker struct {
	Kind BlockMarkerKind

	Header       *BlockHeader
	Footer       *BlockFooter
	UpdateParent *UpdateParent
	GenesisCert  *GenesisCertMarker
}

type BlockMarkerKind uint8

const (
	MarkerBlockFooter BlockMarkerKind = iota
	MarkerBlockHeader
	MarkerUpdateParent
	MarkerGenesisCertificate
)

// BlockHeader is sent at the start of an Alpenglow slot.
type BlockHeader struct {
	ParentSlot    uint64
	ParentBlockID solana.Hash
}

// UpdateParent switches the double-merkle parent commitment mid-slot.
type UpdateParent struct {
	NewParentSlot    uint64
	NewParentBlockID solana.Hash
}

// BlockFooter is sent at slot end with bank hash and finalization certs.
type BlockFooter struct {
	BankHash               solana.Hash
	BlockProducerTimeNanos uint64
	BlockUserAgent         []byte
	BlockFinalCert         []byte
	SkipRewardCert         []byte
	NotarRewardCert        []byte
}

// GenesisCertMarker attests genesis block finalization during migration.
type GenesisCertMarker struct {
	Slot         uint64
	BlockID      solana.Hash
	BLSSignature [192]byte
	Bitmap       []byte
}

// VotesAggregate is a compressed BLS aggregate plus stake bitmap.
type VotesAggregate struct {
	Signature [96]byte
	Bitmap    []byte
}

// BlockFinalizationCert binds a finalized block to vote aggregates.
type BlockFinalizationCert struct {
	Slot           uint64
	BlockID        solana.Hash
	FinalAggregate VotesAggregate
	NotarAggregate *VotesAggregate
}

func NewEntryBatch(entries []Entry) (BlockComponent, error) {
	if len(entries) == 0 {
		return BlockComponent{}, ErrEmptyEntryBatch
	}
	batch := make([]Entry, len(entries))
	copy(batch, entries)
	return BlockComponent{EntryBatch: batch}, nil
}

func NewBlockHeader(parentSlot uint64, parentBlockID solana.Hash) BlockComponent {
	return BlockComponent{
		Marker: &BlockMarker{
			Kind: MarkerBlockHeader,
			Header: &BlockHeader{
				ParentSlot:    parentSlot,
				ParentBlockID: parentBlockID,
			},
		},
	}
}

func NewBlockFooter(footer BlockFooter) BlockComponent {
	f := footer
	return BlockComponent{
		Marker: &BlockMarker{
			Kind:   MarkerBlockFooter,
			Footer: &f,
		},
	}
}

func NewUpdateParent(parentSlot uint64, parentBlockID solana.Hash) BlockComponent {
	return BlockComponent{
		Marker: &BlockMarker{
			Kind: MarkerUpdateParent,
			UpdateParent: &UpdateParent{
				NewParentSlot:    parentSlot,
				NewParentBlockID: parentBlockID,
			},
		},
	}
}

func (c BlockComponent) IsEntryBatch() bool {
	return len(c.EntryBatch) > 0
}

func (c BlockComponent) IsMarker() bool {
	return c.Marker != nil
}

func (c BlockComponent) IsEmptyEntryBatchAbort() bool {
	return len(c.EntryBatch) == 0 && c.Marker == nil
}

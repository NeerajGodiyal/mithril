package alpenglow

import (
	"fmt"
	"math/bits"
	"time"

	"github.com/gagliardetto/solana-go"
)

type BlockID struct {
	Slot uint64      `json:"slot"`
	Hash solana.Hash `json:"hash"`
}

func (b BlockID) IsZero() bool {
	return b.Slot == 0 && b.Hash == (solana.Hash{})
}

func (b BlockID) HasHash() bool {
	return b.Hash != (solana.Hash{})
}

func (b BlockID) String() string {
	return fmt.Sprintf("%d:%s", b.Slot, b.Hash)
}

type Fraction struct {
	Numerator   uint64 `json:"numerator"`
	Denominator uint64 `json:"denominator"`
}

var (
	CertificateThreshold        = FractionFromPercentage(60)
	FastFinalizeThreshold       = FractionFromPercentage(80)
	GenesisCertificateThreshold = FractionFromPercentage(82)
)

func NewFraction(numerator, denominator uint64) Fraction {
	return Fraction{Numerator: numerator, Denominator: denominator}
}

func FractionFromPercentage(pct uint64) Fraction {
	return NewFraction(pct, 100)
}

func (f Fraction) Valid() bool {
	return f.Denominator != 0
}

func (f Fraction) CompareStake(stake, totalStake uint64) (int, error) {
	if !f.Valid() {
		return 0, fmt.Errorf("invalid zero-denominator fraction")
	}
	if totalStake == 0 {
		return 0, fmt.Errorf("total stake is zero")
	}

	lhsHi, lhsLo := bits.Mul64(stake, f.Denominator)
	rhsHi, rhsLo := bits.Mul64(totalStake, f.Numerator)
	if lhsHi < rhsHi {
		return -1, nil
	}
	if lhsHi > rhsHi {
		return 1, nil
	}
	if lhsLo < rhsLo {
		return -1, nil
	}
	if lhsLo > rhsLo {
		return 1, nil
	}
	return 0, nil
}

func (f Fraction) Meets(stake, totalStake uint64) (bool, error) {
	cmp, err := f.CompareStake(stake, totalStake)
	return cmp >= 0, err
}

type VoteType string

const (
	VoteTypeFinalize         VoteType = "finalize"
	VoteTypeNotarize         VoteType = "notarize"
	VoteTypeNotarizeFallback VoteType = "notarize-fallback"
	VoteTypeSkip             VoteType = "skip"
	VoteTypeSkipFallback     VoteType = "skip-fallback"
	VoteTypeGenesis          VoteType = "genesis"
)

func (t VoteType) Valid() bool {
	switch t {
	case VoteTypeFinalize, VoteTypeNotarize, VoteTypeNotarizeFallback, VoteTypeSkip, VoteTypeSkipFallback, VoteTypeGenesis:
		return true
	default:
		return false
	}
}

type Vote struct {
	Type      VoteType    `json:"type"`
	Slot      uint64      `json:"slot"`
	BlockHash solana.Hash `json:"block_hash,omitempty"`
}

func NewNotarizationVote(slot uint64, blockHash solana.Hash) Vote {
	return Vote{Type: VoteTypeNotarize, Slot: slot, BlockHash: blockHash}
}

func NewFinalizationVote(slot uint64) Vote {
	return Vote{Type: VoteTypeFinalize, Slot: slot}
}

func NewSkipVote(slot uint64) Vote {
	return Vote{Type: VoteTypeSkip, Slot: slot}
}

func NewNotarizationFallbackVote(slot uint64, blockHash solana.Hash) Vote {
	return Vote{Type: VoteTypeNotarizeFallback, Slot: slot, BlockHash: blockHash}
}

func NewSkipFallbackVote(slot uint64) Vote {
	return Vote{Type: VoteTypeSkipFallback, Slot: slot}
}

func NewGenesisVote(slot uint64, blockHash solana.Hash) Vote {
	return Vote{Type: VoteTypeGenesis, Slot: slot, BlockHash: blockHash}
}

func (v Vote) Block() (BlockID, bool) {
	switch v.Type {
	case VoteTypeNotarize, VoteTypeNotarizeFallback, VoteTypeGenesis:
		return BlockID{Slot: v.Slot, Hash: v.BlockHash}, true
	default:
		return BlockID{}, false
	}
}

func (v Vote) ValidateBasic() error {
	if !v.Type.Valid() {
		return fmt.Errorf("invalid vote type %q", v.Type)
	}
	return nil
}

type VoteMessage struct {
	Vote      Vote      `json:"vote"`
	Signature []byte    `json:"signature,omitempty"`
	Rank      uint16    `json:"rank"`
	At        time.Time `json:"at,omitempty"`
}

func (m VoteMessage) Key() VoteMessageKey {
	return VoteMessageKey{
		Type:      m.Vote.Type,
		Slot:      m.Vote.Slot,
		BlockHash: m.Vote.BlockHash,
		Rank:      m.Rank,
	}
}

func (m VoteMessage) ValidateBasic() error {
	return m.Vote.ValidateBasic()
}

type VoteMessageKey struct {
	Type      VoteType
	Slot      uint64
	BlockHash solana.Hash
	Rank      uint16
}

type CertificateType string

const (
	CertificateFinalize         CertificateType = "finalize"
	CertificateFinalizeFast     CertificateType = "finalize-fast"
	CertificateNotarize         CertificateType = "notarize"
	CertificateNotarizeFallback CertificateType = "notarize-fallback"
	CertificateSkip             CertificateType = "skip"
	CertificateGenesis          CertificateType = "genesis"
)

func (t CertificateType) Valid() bool {
	switch t {
	case CertificateFinalize, CertificateFinalizeFast, CertificateNotarize, CertificateNotarizeFallback, CertificateSkip, CertificateGenesis:
		return true
	default:
		return false
	}
}

func (t CertificateType) HasBlock() bool {
	switch t {
	case CertificateFinalizeFast, CertificateNotarize, CertificateNotarizeFallback, CertificateGenesis:
		return true
	default:
		return false
	}
}

func (t CertificateType) IsFinalization() bool {
	return t == CertificateFinalize || t == CertificateFinalizeFast
}

func (t CertificateType) RequiredThreshold() Fraction {
	switch t {
	case CertificateFinalizeFast:
		return FastFinalizeThreshold
	case CertificateGenesis:
		return GenesisCertificateThreshold
	default:
		return CertificateThreshold
	}
}

func (t CertificateType) SourceVoteTypes() []VoteType {
	switch t {
	case CertificateNotarize:
		return []VoteType{VoteTypeNotarize}
	case CertificateNotarizeFallback:
		return []VoteType{VoteTypeNotarize, VoteTypeNotarizeFallback}
	case CertificateFinalizeFast:
		return []VoteType{VoteTypeNotarize}
	case CertificateFinalize:
		return []VoteType{VoteTypeFinalize}
	case CertificateSkip:
		return []VoteType{VoteTypeSkip, VoteTypeSkipFallback}
	case CertificateGenesis:
		return []VoteType{VoteTypeGenesis}
	default:
		return nil
	}
}

type Certificate struct {
	Type              CertificateType `json:"type"`
	Slot              uint64          `json:"slot"`
	BlockHash         solana.Hash     `json:"block_hash,omitempty"`
	Signature         []byte          `json:"signature,omitempty"`
	Bitmap            []byte          `json:"bitmap,omitempty"`
	IncludedStake     uint64          `json:"included_stake,omitempty"`
	TotalStake        uint64          `json:"total_stake,omitempty"`
	StakeVerified     bool            `json:"stake_verified,omitempty"`
	SignatureVerified bool            `json:"signature_verified,omitempty"`
	At                time.Time       `json:"at,omitempty"`
}

func (c Certificate) Key() CertificateKey {
	return CertificateKey{
		Type:      c.Type,
		Slot:      c.Slot,
		BlockHash: c.BlockHash,
	}
}

func (c Certificate) Block() (BlockID, bool) {
	if !c.Type.HasBlock() {
		return BlockID{}, false
	}
	return BlockID{Slot: c.Slot, Hash: c.BlockHash}, true
}

func (c Certificate) ValidateBasic() error {
	if !c.Type.Valid() {
		return fmt.Errorf("invalid certificate type %q", c.Type)
	}
	if c.TotalStake == 0 && c.IncludedStake != 0 {
		return fmt.Errorf("certificate has included stake but no total stake")
	}
	if c.TotalStake != 0 {
		ok, err := c.Type.RequiredThreshold().Meets(c.IncludedStake, c.TotalStake)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s certificate for slot %d has insufficient stake: %d/%d below required %d/%d",
				c.Type, c.Slot, c.IncludedStake, c.TotalStake, c.Type.RequiredThreshold().Numerator, c.Type.RequiredThreshold().Denominator)
		}
	}
	return nil
}

type CertificateKey struct {
	Type      CertificateType
	Slot      uint64
	BlockHash solana.Hash
}

type Message struct {
	Vote         *VoteMessage `json:"vote,omitempty"`
	Certificate  *Certificate `json:"certificate,omitempty"`
	ShredVersion uint16       `json:"shred_version,omitempty"`
}

func (m Message) Slot() uint64 {
	if m.Vote != nil {
		return m.Vote.Vote.Slot
	}
	if m.Certificate != nil {
		return m.Certificate.Slot
	}
	return 0
}

func NewVoteMessage(vote Vote, signature []byte, rank uint16) Message {
	msg := VoteMessage{Vote: vote, Signature: signature, Rank: rank}
	return Message{Vote: &msg}
}

func NewCertificateMessage(cert Certificate) Message {
	return Message{Certificate: &cert}
}

func (m Message) ValidateBasic() error {
	switch {
	case m.Vote != nil && m.Certificate != nil:
		return fmt.Errorf("message cannot contain both vote and certificate")
	case m.Vote != nil:
		return m.Vote.ValidateBasic()
	case m.Certificate != nil:
		return m.Certificate.ValidateBasic()
	default:
		return fmt.Errorf("empty alpenglow consensus message")
	}
}

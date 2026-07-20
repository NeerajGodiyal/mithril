package block

import (
	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type Block struct {
	Slot                                uint64
	ParentSlot                          uint64
	SourceParentSlot                    uint64 // Ingress parent slot from the block source; replay may later rewrite ParentSlot.
	BlockHeight                         uint64
	Epoch                               uint64
	Transactions                        []*solana.Transaction
	Versions                            []uint8
	Entries                             []*TxEntry
	BankHash                            [32]byte
	EahWorkaroundBankhash               []byte
	HasEahWorkaround                    bool
	ParentBankhash                      [32]byte
	AcctsLtHash                         *lthash.LtHash
	NumSignatures                       uint64 // signatures carried by this block; replay rejects duplicate transaction messages
	PrevNumSignatures                   uint64 // signatures processed in the parent bank (fee-governor input)
	InitialPreviousLamportsPerSignature uint64
	Blockhash                           [32]byte
	AlpenglowBlockID                    [32]byte // Turbine Merkle-root block id used by Alpenglow/Votor.
	HasAlpenglowBlockID                 bool
	AlpenglowParentBlockID              [32]byte // parent's Merkle-root block id (header/update-parent marker)
	HasAlpenglowParentBlockID           bool
	AlpenglowLastChainedRoot            [32]byte // Last data-shred Merkle root, chained into child slots.
	HasAlpenglowLastChainedRoot         bool
	AlpenglowFinalCert                  []byte // raw footer final_cert bytes (finalization cert for an earlier slot), decoded in replay
	ExpectedBankhash                    [32]byte
	HasExpectedBankhash                 bool
	TxMetas                             []*rpc.TransactionMeta
	Leader                              solana.PublicKey
	BlockReward                         *BlockRewardsInfo
	LastBlockhash                       [32]byte
	UnixTimestamp                       int64
	EpochStakesPerVoteAcct              map[solana.PublicKey]uint64
	VoteTimestamps                      map[solana.PublicKey]sealevel.BlockTimestamp
	TotalEpochStake                     uint64
	Features                            *features.Features
	UpdatedAccts                        []solana.PublicKey
	ParentEpochUpdatedAccts             []*accounts.Account
	EpochUpdatedAccts                   []*accounts.Account
	Rewards                             []rpc.BlockReward
	NumRewardPartitions                 uint64
	LatestEvictedBlockhash              [32]byte
	PrevFeeRateGovernor                 *sealevel.FeeRateGovernor
	FeeRateGovernor                     *sealevel.FeeRateGovernor
	FromLiveStream                      bool
	FromLocalProduction                 bool
	IsSkipped                           bool // True for slots that were skipped by the leader
	// transactionSignaturesVerified is deliberately private: it is a trust
	// marker set only by an in-process source after it has verified every
	// transaction signature. It must not be accepted from JSON/file input.
	transactionSignaturesVerified bool
	SkipRewardCert                []byte
	NotarRewardCert               []byte
	BlockFinalCert                []byte
	FooterProducerTimeNanos       uint64
	HasAlpenglowFooter            bool
	AlpenglowShredVersion         uint16

	// Shred-path observability (zero when the block did not come from shreds —
	// RPC/file blocks must not fabricate these). "Full" follows Agave's
	// SlotMeta/is_full language: all data shreds present, block reconstructable
	// — NOT finalized/consensus-safe.
	ShredFirstNanos int64 // wall clock (unix nanos) of the first accepted shred for the slot
	ShredFullNanos  int64 // wall clock (unix nanos) when the slot became full
	RepairedShreds  int   // data shreds obtained via repair rather than turbine
}

// MarkTransactionSignaturesVerified records that every transaction signature
// in this exact in-memory block has already been verified. Callers must set it
// only after successful verification and must not subsequently mutate signed
// transaction data. Signature-preserving transformations such as resolving
// address-table lookups remain safe.
func (b *Block) MarkTransactionSignaturesVerified() {
	if b != nil {
		b.transactionSignaturesVerified = true
	}
}

// TransactionSignaturesVerified reports whether this exact in-memory block
// crossed a trusted transaction-signature verification boundary. The marker is
// intentionally not serialized; replay re-verifies after any serialization.
func (b *Block) TransactionSignaturesVerified() bool {
	return b != nil && b.transactionSignaturesVerified
}

func (b *Block) FixupTxVersions() {
	if len(b.Versions) == 0 {
		return
	}
	for idx, tx := range b.Transactions {
		tx.Message.SetVersion(solana.MessageVersion(b.Versions[idx]))
	}
}

type TxEntry struct {
	NumHashes uint64
	Hash      []byte
	Indices   []uint64
}

type BlockRewardsInfo struct {
	Leader      solana.PublicKey
	Lamports    uint64
	PostBalance uint64
}

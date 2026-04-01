package epochstakes

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/gagliardetto/solana-go"
)

type EpochStakesCache struct {
	pubkeys         []solana.PublicKey
	stakeCache      map[uint64]map[solana.PublicKey]uint64
	voteAcctCache   map[uint64]map[solana.PublicKey]*VoteAccount
	totalStakeCache map[uint64]uint64
}

type VoteAccount struct {
	Lamports          uint64
	Data              []byte
	NodePubkey        solana.PublicKey
	LastTimestampTs   int64
	LastTimestampSlot uint64
	Owner             solana.PublicKey
	Executable        byte
	RentEpoch         uint64

	voteStateOnce sync.Once                   `json:"-"`
	voteState     *sealevel.VoteStateVersions `json:"-"`
	voteStateErr  error                       `json:"-"`
}

func NewEpochStakesCache() *EpochStakesCache {
	return &EpochStakesCache{stakeCache: make(map[uint64]map[solana.PublicKey]uint64),
		voteAcctCache:   make(map[uint64]map[solana.PublicKey]*VoteAccount),
		totalStakeCache: make(map[uint64]uint64)}
}

func (cache *EpochStakesCache) ensureEpoch(epoch uint64) {
	if _, exists := cache.stakeCache[epoch]; !exists {
		cache.stakeCache[epoch] = make(map[solana.PublicKey]uint64)
	}
	if _, exists := cache.voteAcctCache[epoch]; !exists {
		cache.voteAcctCache[epoch] = make(map[solana.PublicKey]*VoteAccount)
	}
}

func (cache *EpochStakesCache) PutEntry(epoch uint64, pubkey solana.PublicKey, stake uint64, voteAcct *VoteAccount) {
	cache.ensureEpoch(epoch)
	cache.stakeCache[epoch][pubkey] = stake
	cache.voteAcctCache[epoch][pubkey] = voteAcct
}

func (cache *EpochStakesCache) PutStake(epoch uint64, pubkey solana.PublicKey, stake uint64) {
	cache.ensureEpoch(epoch)
	cache.stakeCache[epoch][pubkey] = stake
}

func (cache *EpochStakesCache) PutVoteAccount(epoch uint64, pubkey solana.PublicKey, voteAcct *VoteAccount) {
	cache.ensureEpoch(epoch)
	cache.voteAcctCache[epoch][pubkey] = voteAcct
}

func (cache *EpochStakesCache) PutTotalEpochStake(epoch uint64, totalStake uint64) {
	cache.totalStakeCache[epoch] = totalStake
}

func (cache *EpochStakesCache) EpochStakes(epoch uint64) map[solana.PublicKey]uint64 {
	return cache.stakeCache[epoch]
}

func (cache *EpochStakesCache) HasEpochStakes(epoch uint64) bool {
	_, exists := cache.stakeCache[epoch]
	return exists
}

func (cache *EpochStakesCache) EpochStakesAccts(epoch uint64) map[solana.PublicKey]*VoteAccount {
	return cache.voteAcctCache[epoch]
}

func (cache *EpochStakesCache) TotalStake(epoch uint64) uint64 {
	return cache.totalStakeCache[epoch]
}

// ClearEpochStakes removes all stakes for a specific epoch.
// Used on resume to force rebuild from AccountsDB.
func (cache *EpochStakesCache) ClearEpochStakes(epoch uint64) {
	delete(cache.stakeCache, epoch)
	delete(cache.voteAcctCache, epoch)
	delete(cache.totalStakeCache, epoch)
}

// PersistedEpochStakes is the JSON-serializable format for epoch stakes.
type PersistedEpochStakes struct {
	Epoch      uint64                      `json:"epoch"`
	TotalStake uint64                      `json:"total_stake"`
	Stakes     map[string]uint64           `json:"stakes"`     // base58 pubkey → stake
	VoteAccts  map[string]*VoteAccountJSON `json:"vote_accts"` // base58 pubkey → metadata
}

// VoteAccountJSON is the JSON-serializable format for vote account metadata.
type VoteAccountJSON struct {
	Lamports          uint64 `json:"lamports"`
	Data              []byte `json:"data,omitempty"`
	NodePubkey        string `json:"node_pubkey"`
	LastTimestampTs   int64  `json:"last_ts"`
	LastTimestampSlot uint64 `json:"last_ts_slot"`
	Owner             string `json:"owner"`
	Executable        byte   `json:"executable"`
	RentEpoch         uint64 `json:"rent_epoch"`
}

// SerializeEpoch serializes the stakes for a single epoch to JSON.
func (cache *EpochStakesCache) SerializeEpoch(epoch uint64) ([]byte, error) {
	stakes := cache.stakeCache[epoch]
	voteAccts := cache.voteAcctCache[epoch]
	totalStake := cache.totalStakeCache[epoch]

	if stakes == nil {
		return nil, fmt.Errorf("no stakes for epoch %d", epoch)
	}

	persisted := PersistedEpochStakes{
		Epoch:      epoch,
		TotalStake: totalStake,
		Stakes:     make(map[string]uint64, len(stakes)),
		VoteAccts:  make(map[string]*VoteAccountJSON, len(voteAccts)),
	}

	for pk, stake := range stakes {
		persisted.Stakes[pk.String()] = stake
	}

	for pk, va := range voteAccts {
		if va != nil {
			persisted.VoteAccts[pk.String()] = &VoteAccountJSON{
				Lamports:          va.Lamports,
				Data:              append([]byte(nil), va.Data...),
				NodePubkey:        va.NodePubkey.String(),
				LastTimestampTs:   va.LastTimestampTs,
				LastTimestampSlot: va.LastTimestampSlot,
				Owner:             va.Owner.String(),
				Executable:        va.Executable,
				RentEpoch:         va.RentEpoch,
			}
		}
	}

	return json.Marshal(persisted)
}

// DeserializeAndLoadEpoch deserializes epoch stakes from JSON and loads into cache.
// Returns the epoch number that was loaded.
func (cache *EpochStakesCache) DeserializeAndLoadEpoch(data []byte) (uint64, error) {
	var persisted PersistedEpochStakes
	if err := json.Unmarshal(data, &persisted); err != nil {
		return 0, fmt.Errorf("failed to unmarshal epoch stakes: %w", err)
	}

	epoch := persisted.Epoch

	// Initialize maps for this epoch
	cache.stakeCache[epoch] = make(map[solana.PublicKey]uint64, len(persisted.Stakes))
	cache.voteAcctCache[epoch] = make(map[solana.PublicKey]*VoteAccount, len(persisted.VoteAccts))
	cache.totalStakeCache[epoch] = persisted.TotalStake

	for pkStr, stake := range persisted.Stakes {
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			return 0, fmt.Errorf("invalid stake pubkey %q for epoch %d: %w", pkStr, epoch, err)
		}
		cache.stakeCache[epoch][pk] = stake
	}

	for pkStr, vaJSON := range persisted.VoteAccts {
		pk, err := solana.PublicKeyFromBase58(pkStr)
		if err != nil {
			return 0, fmt.Errorf("invalid vote acct pubkey %q for epoch %d: %w", pkStr, epoch, err)
		}
		nodePubkey, err := solana.PublicKeyFromBase58(vaJSON.NodePubkey)
		if err != nil {
			return 0, fmt.Errorf("invalid node pubkey %q for vote acct %s epoch %d: %w", vaJSON.NodePubkey, pkStr, epoch, err)
		}
		owner, err := solana.PublicKeyFromBase58(vaJSON.Owner)
		if err != nil {
			return 0, fmt.Errorf("invalid owner pubkey %q for vote acct %s epoch %d: %w", vaJSON.Owner, pkStr, epoch, err)
		}
		cache.voteAcctCache[epoch][pk] = &VoteAccount{
			Lamports:          vaJSON.Lamports,
			Data:              append([]byte(nil), vaJSON.Data...),
			NodePubkey:        nodePubkey,
			LastTimestampTs:   vaJSON.LastTimestampTs,
			LastTimestampSlot: vaJSON.LastTimestampSlot,
			Owner:             owner,
			Executable:        vaJSON.Executable,
			RentEpoch:         vaJSON.RentEpoch,
		}
	}

	return epoch, nil
}

// GetAllEpochs returns a list of all epochs in the cache.
func (cache *EpochStakesCache) GetAllEpochs() []uint64 {
	epochs := make([]uint64, 0, len(cache.stakeCache))
	for epoch := range cache.stakeCache {
		epochs = append(epochs, epoch)
	}
	return epochs
}

func NewVoteAccountFromAccount(acct *accounts.Account) (*VoteAccount, error) {
	if acct == nil {
		return nil, fmt.Errorf("nil vote account")
	}

	voteState, err := sealevel.UnmarshalVersionedVoteState(acct.Data)
	if err != nil {
		return nil, fmt.Errorf("unmarshal vote state: %w", err)
	}

	voteAcct := &VoteAccount{
		Lamports:   acct.Lamports,
		Data:       append([]byte(nil), acct.Data...),
		NodePubkey: voteState.NodePubkey(),
		Owner:      solana.PublicKeyFromBytes(acct.Owner[:]),
		RentEpoch:  acct.RentEpoch,
		voteState:  voteState,
	}
	if acct.Executable {
		voteAcct.Executable = 1
	}
	if ts := voteState.LastTimestamp(); ts != nil {
		voteAcct.LastTimestampTs = ts.Timestamp
		voteAcct.LastTimestampSlot = ts.Slot
	}
	voteAcct.voteStateOnce.Do(func() {})
	return voteAcct, nil
}

func (voteAcct *VoteAccount) Clone() *VoteAccount {
	if voteAcct == nil {
		return nil
	}

	clone := *voteAcct
	clone.Data = append([]byte(nil), voteAcct.Data...)
	clone.voteStateOnce = sync.Once{}
	if voteAcct.voteState != nil || voteAcct.voteStateErr != nil || len(voteAcct.Data) == 0 {
		clone.voteState = voteAcct.voteState
		clone.voteStateErr = voteAcct.voteStateErr
		clone.voteStateOnce.Do(func() {})
	}
	return &clone
}

func (voteAcct *VoteAccount) VoteState() (*sealevel.VoteStateVersions, error) {
	if voteAcct == nil {
		return nil, fmt.Errorf("nil vote account")
	}

	voteAcct.voteStateOnce.Do(func() {
		if len(voteAcct.Data) == 0 {
			voteAcct.voteStateErr = fmt.Errorf("vote account has no data")
			return
		}
		voteAcct.voteState, voteAcct.voteStateErr = sealevel.UnmarshalVersionedVoteState(voteAcct.Data)
	})

	return voteAcct.voteState, voteAcct.voteStateErr
}

func (voteAcct *VoteAccount) NodePubkeyOrZero() solana.PublicKey {
	if voteAcct == nil {
		return solana.PublicKey{}
	}
	if voteState, err := voteAcct.VoteState(); err == nil && voteState != nil {
		return voteState.NodePubkey()
	}
	return voteAcct.NodePubkey
}

func (voteAcct *VoteAccount) ToAccount(pubkey solana.PublicKey, slot uint64) *accounts.Account {
	if voteAcct == nil {
		return nil
	}

	var owner [32]byte
	copy(owner[:], voteAcct.Owner[:])

	return &accounts.Account{
		Slot:       slot,
		Key:        pubkey,
		Lamports:   voteAcct.Lamports,
		Data:       append([]byte(nil), voteAcct.Data...),
		Owner:      owner,
		Executable: voteAcct.Executable != 0,
		RentEpoch:  voteAcct.RentEpoch,
	}
}

package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/Overclock-Validator/mithril/pkg/wincode"
)

// EpochInflationState stores per-epoch validator inflation metadata for Alpenglow vote rewards.
type EpochInflationState struct {
	MaxPossibleValidatorReward uint64
	SlotsPerEpoch              uint64
	Epoch                      uint64
}

// EpochInflationAccountState mirrors Agave's vote-reward metadata account.
type EpochInflationAccountState struct {
	Current EpochInflationState
	Prev    *EpochInflationState
}

func decodeEpochInflationState(r *wincode.Reader) (EpochInflationState, error) {
	maxReward, err := r.ReadU64()
	if err != nil {
		return EpochInflationState{}, err
	}
	slotsPerEpoch, err := r.ReadU64()
	if err != nil {
		return EpochInflationState{}, err
	}
	epoch, err := r.ReadU64()
	if err != nil {
		return EpochInflationState{}, err
	}
	return EpochInflationState{
		MaxPossibleValidatorReward: maxReward,
		SlotsPerEpoch:              slotsPerEpoch,
		Epoch:                      epoch,
	}, nil
}

func decodeEpochInflationAccountState(data []byte) (EpochInflationAccountState, error) {
	r := wincode.NewReader(data)
	current, err := decodeEpochInflationState(r)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode current epoch inflation: %w", err)
	}
	if r.Remaining() < 1 {
		return EpochInflationAccountState{}, fmt.Errorf("decode prev tag: truncated")
	}
	tagBytes, err := r.ReadBytes(1)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode prev tag: %w", err)
	}
	var prev *EpochInflationState
	switch tagBytes[0] {
	case 0:
	case 1:
		state, err := decodeEpochInflationState(r)
		if err != nil {
			return EpochInflationAccountState{}, fmt.Errorf("decode prev epoch inflation: %w", err)
		}
		prev = &state
	default:
		return EpochInflationAccountState{}, fmt.Errorf("invalid prev tag %d", tagBytes[0])
	}
	if err := r.EnsureEOF(); err != nil {
		return EpochInflationAccountState{}, err
	}
	return EpochInflationAccountState{Current: current, Prev: prev}, nil
}

func (s EpochInflationAccountState) epochState(epoch uint64) (EpochInflationState, bool) {
	if s.Current.Epoch == epoch {
		return s.Current, true
	}
	if s.Prev != nil && s.Prev.Epoch == epoch {
		return *s.Prev, true
	}
	return EpochInflationState{}, false
}

func loadEpochInflationAccountState(acctsDb *accountsdb.AccountsDb, slot uint64) (EpochInflationAccountState, error) {
	acct, err := acctsDb.GetAccount(slot, VoteRewardAccountAddr())
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("load vote reward account at slot %d: %w", slot, err)
	}
	if len(acct.Data) == 0 {
		return EpochInflationAccountState{}, fmt.Errorf("vote reward account missing at slot %d", slot)
	}
	state, err := decodeEpochInflationAccountState(acct.Data)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode vote reward account at slot %d: %w", slot, err)
	}
	return state, nil
}

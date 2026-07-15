package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/wincode"
)

// EpochInflationState stores the per-epoch reward budget metadata used by
// Alpenglow vote rewards.
type EpochInflationState struct {
	MaxPossibleValidatorReward uint64
	SlotsPerEpoch              uint64
	Epoch                      uint64
}

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
	return EpochInflationState{MaxPossibleValidatorReward: maxReward, SlotsPerEpoch: slotsPerEpoch, Epoch: epoch}, nil
}

func decodeEpochInflationAccountState(data []byte) (EpochInflationAccountState, error) {
	r := wincode.NewReader(data)
	current, err := decodeEpochInflationState(r)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode current epoch inflation: %w", err)
	}
	tag, err := r.ReadBytes(1)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode previous epoch inflation tag: %w", err)
	}
	var prev *EpochInflationState
	switch tag[0] {
	case 0:
	case 1:
		state, err := decodeEpochInflationState(r)
		if err != nil {
			return EpochInflationAccountState{}, fmt.Errorf("decode previous epoch inflation: %w", err)
		}
		prev = &state
	default:
		return EpochInflationAccountState{}, fmt.Errorf("invalid previous epoch inflation tag %d", tag[0])
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

func loadEpochInflationAccountStateForReplay(slotCtx *sealevel.SlotCtx) (EpochInflationAccountState, error) {
	if slotCtx == nil {
		return EpochInflationAccountState{}, fmt.Errorf("missing slot context")
	}
	acct, err := slotCtx.GetAccountFromAccountsDb(VoteRewardAccountAddr())
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("load vote reward account at parent slot %d: %w", slotCtx.ParentSlot, err)
	}
	return decodeEpochInflationAccountFromAcct(acct, slotCtx.ParentSlot)
}

func decodeEpochInflationAccountFromAcct(acct *accounts.Account, slot uint64) (EpochInflationAccountState, error) {
	if acct == nil || len(acct.Data) == 0 {
		return EpochInflationAccountState{}, fmt.Errorf("vote reward account missing at slot %d", slot)
	}
	state, err := decodeEpochInflationAccountState(acct.Data)
	if err != nil {
		return EpochInflationAccountState{}, fmt.Errorf("decode vote reward account at slot %d: %w", slot, err)
	}
	return state, nil
}

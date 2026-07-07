package replay

import (
	"encoding/base64"
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/state"
	bin "github.com/gagliardetto/binary"
	"github.com/mr-tron/base58"
)

// DecodeRecentBlockhashes converts persisted state.BlockhashEntry entries back to
// the sealevel sysvar (inverse of EncodeRecentBlockhashes).
func DecodeRecentBlockhashes(entries []state.BlockhashEntry) sealevel.SysvarRecentBlockhashes {
	result := make(sealevel.SysvarRecentBlockhashes, 0, len(entries))
	dropped := 0
	for _, entry := range entries {
		hashBytes, err := base58.Decode(entry.Blockhash)
		if err != nil || len(hashBytes) != 32 {
			dropped++
			continue
		}
		var blockhash [32]byte
		copy(blockhash[:], hashBytes)
		result = append(result, sealevel.RecentBlockHashesEntry{
			Blockhash:     blockhash,
			FeeCalculator: sealevel.FeeCalculator{LamportsPerSignature: entry.LamportsPerSignature},
		})
	}
	if dropped > 0 {
		mlog.Log.Errorf("dropped %d/%d RecentBlockhashes entries due to invalid base58 - state file may be corrupted", dropped, len(entries))
	}
	return result
}

// DecodeSlotHashes converts persisted state.SlotHashEntry entries back to the
// sealevel sysvar (inverse of EncodeSlotHashes).
func DecodeSlotHashes(entries []state.SlotHashEntry) sealevel.SysvarSlotHashes {
	result := make(sealevel.SysvarSlotHashes, 0, len(entries))
	dropped := 0
	for _, entry := range entries {
		hashBytes, err := base58.Decode(entry.Hash)
		if err != nil || len(hashBytes) != 32 {
			dropped++
			continue
		}
		var hash [32]byte
		copy(hash[:], hashBytes)
		result = append(result, sealevel.SlotHash{
			Slot: entry.Slot,
			Hash: hash,
		})
	}
	if dropped > 0 {
		mlog.Log.Errorf("dropped %d/%d SlotHashes entries due to invalid base58 - state file may be corrupted", dropped, len(entries))
	}
	return result
}

// seedBranchSysvarsFromContext seeds the slot's branch sysvar cache with the
// carried-forward families (Clock, SlotHashes, RecentBlockhashes) from a branch's
// end-of-slot resume context. This is how a block executing on a fork reads ITS
// parent's sysvar state rather than whatever the linear tip left in the global
// cache. The context is the same deep-copied snapshot the fork coordinator holds
// per branch, so seeding never aliases another branch's live state.
func seedBranchSysvarsFromContext(slotCtx *sealevel.SlotCtx, ctx *state.ResumeContext) error {
	if slotCtx == nil || ctx == nil {
		return fmt.Errorf("branch sysvar seed: nil slot context or resume context")
	}

	var clock *sealevel.SysvarClock
	if ctx.Clock != "" {
		clockBytes, err := base64.StdEncoding.DecodeString(ctx.Clock)
		if err != nil {
			return fmt.Errorf("branch sysvar seed: clock base64: %w", err)
		}
		var c sealevel.SysvarClock
		if err := c.UnmarshalWithDecoder(bin.NewBinDecoder(clockBytes)); err != nil {
			return fmt.Errorf("branch sysvar seed: clock decode: %w", err)
		}
		clock = &c
	}

	var slotHashes *sealevel.SysvarSlotHashes
	if len(ctx.SlotHashes) > 0 {
		sh := DecodeSlotHashes(ctx.SlotHashes)
		slotHashes = &sh
	}

	var recentBlockhashes *sealevel.SysvarRecentBlockhashes
	if len(ctx.RecentBlockhashes) > 0 {
		rbh := DecodeRecentBlockhashes(ctx.RecentBlockhashes)
		recentBlockhashes = &rbh
	}

	slotCtx.InitBranchSysvars(clock, slotHashes, recentBlockhashes)
	return nil
}

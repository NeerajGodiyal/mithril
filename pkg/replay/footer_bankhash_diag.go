package replay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/bankhash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	b "github.com/Overclock-Validator/mithril/pkg/block"
	"github.com/Overclock-Validator/mithril/pkg/base58"
)

func replayBankHashDetailInput(
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	modifiedAccts []*accounts.Account,
) bankhash.SlotDetailsInput {
	var parentBankHash [32]byte
	var lastBlockhash [32]byte
	if block != nil {
		parentBankHash = block.ParentBankhash
		lastBlockhash = block.Blockhash
	}
	return bankhash.SlotDetailsInput{
		Slot:           slotCtx.Slot,
		BankHash:       append([]byte(nil), slotCtx.FinalBankhash...),
		ParentBankHash: parentBankHash,
		SignatureCount: slotCtx.NumSignatures,
		LastBlockhash:  lastBlockhash,
		AcctsLtHash:    slotCtx.AcctsLtHash,
		Accounts:       modifiedAccts,
	}
}

// logFooterBankHashMismatchDiagnostics emits bank-hash component fields comparable to
// Agave getBlockAG replayDiag, plus speculative-store context. Agave replayDiag is only
// available while the bank remains in BankForks; footer bankHash is always available
// via getBlockAG alpenglowFooter on a synced validator RPC.
func logFooterBankHashMismatchDiagnostics(
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	modifiedAccts []*accounts.Account,
	spec *SpeculativeReplay,
	acctsDbDir string,
) {
	if slotCtx == nil || block == nil {
		return
	}

	detailInput := replayBankHashDetailInput(slotCtx, block, modifiedAccts)
	summary := bankhash.SnapshotSlotDetails(detailInput)
	footer := base58.Encode(block.ExpectedBankhash[:])

	mlog.Log.Errorf(
		"footer bank hash mismatch components slot=%d footer=%s computed=%s parent_bankhash=%s sigs=%d last_blockhash=%s lt_checksum=%s modified_accounts=%d parent_slot=%d source_parent_slot=%d",
		block.Slot,
		footer,
		summary.BankHash,
		summary.ParentBankHash,
		summary.SignatureCount,
		summary.LastBlockhash,
		summary.AccountsLtHashChecksum,
		summary.ModifiedAccountCount,
		block.ParentSlot,
		block.SourceParentSlot,
	)

	if clock := sealevel.SysvarCache.Clock.Sysvar; clock != nil {
		mlog.Log.Errorf(
			"footer bank hash mismatch clock slot=%d clock_slot=%d epoch=%d unix_timestamp=%d epoch_start_timestamp=%d leader_schedule_epoch=%d",
			block.Slot,
			clock.Slot,
			clock.Epoch,
			clock.UnixTimestamp,
			clock.EpochStartTimestamp,
			clock.LeaderScheduleEpoch,
		)
	}

	if spec != nil && spec.Enabled() {
		mlog.Log.Errorf(
			"footer bank hash mismatch speculative finalized_slot=%d parent_uses_store=%t speculative_layers=%d",
			spec.FinalizedSlot(),
			spec.UseStoreForParent(block.ParentSlot),
			spec.LayerCount(),
		)
	}

	mlog.Log.Errorf(
		"footer bank hash ground truth: getBlockAG alpenglowFooter.bankHash for slot %d (expect %s); replayDiag parent/sigs/lt_checksum only when bank still in BankForks — probe: RPC=http://127.0.0.1:8901 bash scripts/probe_getblockag_diag.sh %d",
		block.Slot,
		footer,
		block.Slot,
	)

	if err := bankhash.WriteMismatchBankHashDetails(acctsDbDir, detailInput); err != nil {
		mlog.Log.Warnf("footer bank hash mismatch: failed to write bank_hash_details: %v", err)
	}

	writeFooterMismatchArtifact(slotCtx, block, modifiedAccts, summary, footer, spec)
}

func writeFooterMismatchArtifact(
	slotCtx *sealevel.SlotCtx,
	block *b.Block,
	modifiedAccts []*accounts.Account,
	summary bankhash.SlotDetailsSnapshot,
	footer string,
	spec *SpeculativeReplay,
) {
	logDir := mlog.GetLogDir()
	if logDir == "" {
		return
	}

	modifiedPubkeys := make([]string, 0, len(modifiedAccts))
	for _, acct := range modifiedAccts {
		if acct != nil {
			modifiedPubkeys = append(modifiedPubkeys, acct.Key.String())
		}
	}
	sort.Strings(modifiedPubkeys)

	artifact := map[string]any{
		"type":       "footer_bankhash_mismatch",
		"run_id":     CurrentRunID,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
		"slot":       block.Slot,
		"components": map[string]any{
			"footer_bankhash":             footer,
			"computed_bankhash":           summary.BankHash,
			"parent_bankhash":             summary.ParentBankHash,
			"signature_count":           summary.SignatureCount,
			"last_blockhash":            summary.LastBlockhash,
			"accounts_lt_hash_checksum": summary.AccountsLtHashChecksum,
			"modified_account_count":    summary.ModifiedAccountCount,
			"modified_pubkeys":          modifiedPubkeys,
		},
		"block":        consensusBlockDiagnostic(block),
		"slot_context": footerMismatchSlotContext(slotCtx),
		"ground_truth": map[string]any{
			"footer_via":      "getBlockAG.alpenglowFooter.bankHash",
			"replay_diag_via": "getBlockAG.replayDiag (only while bank in BankForks)",
			"probe_script":    fmt.Sprintf("RPC=http://127.0.0.1:8901 bash scripts/probe_getblockag_diag.sh %d", block.Slot),
		},
	}
	if spec != nil && spec.Enabled() {
		artifact["speculative"] = map[string]any{
			"finalized_slot":    spec.FinalizedSlot(),
			"layer_count":       spec.LayerCount(),
			"parent_uses_store": spec.UseStoreForParent(block.ParentSlot),
		}
	}

	dir := filepath.Join(logDir, "footer_mismatch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		mlog.Log.Warnf("footer mismatch artifact: mkdir %s: %v", dir, err)
		return
	}
	filename := fmt.Sprintf("slot_%d_computed_%s.json", block.Slot, summary.BankHash)
	path := filepath.Join(dir, filename)
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		mlog.Log.Warnf("footer mismatch artifact: marshal: %v", err)
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		mlog.Log.Warnf("footer mismatch artifact: write %s: %v", path, err)
		return
	}
	mlog.Log.Errorf("footer bank hash mismatch artifact: %s", path)
}

func footerMismatchSlotContext(slotCtx *sealevel.SlotCtx) map[string]any {
	if slotCtx == nil {
		return nil
	}
	return map[string]any{
		"slot":                   slotCtx.Slot,
		"parent_slot":            slotCtx.ParentSlot,
		"final_bankhash":         consensusByteHashString(slotCtx.FinalBankhash),
		"accts_lthash_checksum":  consensusLtHashChecksum(slotCtx.AcctsLtHash),
		"num_signatures":         slotCtx.NumSignatures,
		"lamports_burnt":         slotCtx.LamportsBurnt,
		"modified_account_count": len(slotCtx.ModifiedAccts),
	}
}

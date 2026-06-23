package bankhash

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/Overclock-Validator/mithril/pkg/base58"
	"github.com/Overclock-Validator/mithril/pkg/lthash"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
	"github.com/Overclock-Validator/mithril/pkg/sealevel"
	"github.com/Overclock-Validator/mithril/pkg/version"
	"github.com/gagliardetto/solana-go"
)

// SlotDetailsInput captures bank-hash components after leader commit or replay.
type SlotDetailsInput struct {
	Slot                    uint64
	BankHash                []byte
	ParentBankHash          [32]byte
	SignatureCount          uint64
	LastBlockhash           [32]byte
	AcctsLtHash             *lthash.LtHash
	Accounts                []*accounts.Account
	FooterProducerTimeNanos *uint64
	CommitFooterTimestamp   *int64
}

type bankHashDetailsFile struct {
	Version             string        `json:"version"`
	AccountDataEncoding string        `json:"account_data_encoding"`
	BankHashDetails     []slotDetails `json:"bank_hash_details"`
}

type slotDetails struct {
	Slot                         uint64          `json:"slot"`
	BankHash                     string          `json:"bank_hash"`
	ParentBankHash               string          `json:"parent_bank_hash"`
	SignatureCount               uint64          `json:"signature_count"`
	LastBlockhash                string          `json:"last_blockhash"`
	AccountsLtHashChecksum       string          `json:"accounts_lt_hash_checksum"`
	Accounts                     []accountDetail `json:"accounts"`
	BlockProducerTimeNanos       *uint64         `json:"block_producer_time_nanos,omitempty"`
	CommitFooterTimestampSeconds *int64          `json:"commit_footer_timestamp_seconds,omitempty"`
}

type accountDetail struct {
	Pubkey     string `json:"pubkey"`
	Owner      string `json:"owner"`
	Lamports   uint64 `json:"lamports"`
	Executable bool   `json:"executable"`
	Data       string `json:"data"`
}

// BuildSlotDetails constructs Agave-compatible bank hash detail components.
func BuildSlotDetails(in SlotDetailsInput) slotDetails {
	details := slotDetails{
		Slot:           in.Slot,
		BankHash:       base58.Encode(in.BankHash),
		ParentBankHash: base58.Encode(in.ParentBankHash[:]),
		SignatureCount: in.SignatureCount,
		LastBlockhash:  base58.Encode(in.LastBlockhash[:]),
		BlockProducerTimeNanos:       in.FooterProducerTimeNanos,
		CommitFooterTimestampSeconds: in.CommitFooterTimestamp,
	}
	if in.AcctsLtHash != nil {
		details.AccountsLtHashChecksum = base58.Encode(in.AcctsLtHash.Checksum())
	}
	details.Accounts = accountDetailsFromModified(in.Accounts)
	return details
}

func accountDetailsFromModified(accts []*accounts.Account) []accountDetail {
	if len(accts) == 0 {
		return nil
	}
	sorted := append([]*accounts.Account(nil), accts...)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i].Key[:], sorted[j].Key[:]) < 0
	})
	out := make([]accountDetail, 0, len(sorted))
	for _, acct := range sorted {
		if acct == nil {
			continue
		}
		out = append(out, accountDetail{
			Pubkey:     acct.Key.String(),
			Owner:      base58.Encode(acct.Owner[:]),
			Lamports:   acct.Lamports,
			Executable: acct.Executable,
			Data:       base64.StdEncoding.EncodeToString(acct.Data),
		})
	}
	return out
}

// WriteLeaderBankHashDetails writes Agave-compatible bank hash details for a locally produced slot.
func WriteLeaderBankHashDetails(accountsDbDir string, in SlotDetailsInput) error {
	if len(in.BankHash) != 32 {
		return fmt.Errorf("bank hash must be 32 bytes")
	}
	details := BuildSlotDetails(in)
	payload := bankHashDetailsFile{
		Version:             fmt.Sprintf("mithril %s (commit:%s)", version.Version, version.GitCommit),
		AccountDataEncoding: "base64",
		BankHashDetails:     []slotDetails{details},
	}

	filename := fmt.Sprintf("%d-%s.json", in.Slot, details.BankHash)
	dir := filepath.Join(accountsDbDir, "bank_hash_details")
	path := filepath.Join(dir, filename)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create bank_hash_details dir: %w", err)
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bank hash details: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write bank hash details: %w", err)
	}
	mlog.Log.Infof("writing bank hash details file: %s", path)
	mlog.Log.Infof(
		"bank hash details slot=%d hash=%s parent=%s sigs=%d last_blockhash=%s lt_checksum=%s accounts=%d producer_nanos=%v commit_ts=%v",
		in.Slot,
		details.BankHash,
		details.ParentBankHash,
		details.SignatureCount,
		details.LastBlockhash,
		details.AccountsLtHashChecksum,
		len(details.Accounts),
		in.FooterProducerTimeNanos,
		in.CommitFooterTimestamp,
	)
	return nil
}

// SlotDetailsFromLeaderCommit builds detail input from a committed leader slot context.
func SlotDetailsFromLeaderCommit(
	slotCtx *sealevel.SlotCtx,
	parentBankHash [32]byte,
	entryBlockhash solana.Hash,
	modifiedAccts []*accounts.Account,
	footerProducerTimeNanos *uint64,
	commitFooterTimestamp *int64,
) SlotDetailsInput {
	in := SlotDetailsInput{
		Slot:                    slotCtx.Slot,
		BankHash:                append([]byte(nil), slotCtx.FinalBankhash...),
		ParentBankHash:          parentBankHash,
		SignatureCount:          slotCtx.NumSignatures,
		LastBlockhash:           entryBlockhash,
		AcctsLtHash:             slotCtx.AcctsLtHash,
		Accounts:                modifiedAccts,
		FooterProducerTimeNanos: footerProducerTimeNanos,
		CommitFooterTimestamp:   commitFooterTimestamp,
	}
	return in
}

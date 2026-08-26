// Package rootedevents defines and durably stages versioned transaction and account events from
// Mithril's rooted promotion boundary. Replay selects a staged sidecar through
// its fold manifest; retention, acknowledgement, and transport remain outside
// replay.
package rootedevents

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
)

const (
	// SchemaVersion is the current rooted-event wire schema.
	SchemaVersion uint32 = 3

	// These limits mirror the runtime values that can reach schema v3. Message
	// bytes are opaque canonical Solana messages, including v1.
	maxAccountDataBytes          = 10 << 20
	maxTransactionMessageBytes   = solana.MaxTransactionSizeV1
	maxTransactionAccountKeys    = 256
	maxTransactionLogBytes       = 10_064
	maxInnerInstructions         = 64
	maxInnerInstructionAccounts  = 255
	maxInnerInstructionDataBytes = 10 << 10
	maxReturnDataBytes           = 1024
)

// Kind identifies the payload carried by an Event.
type Kind string

const (
	// TransactionExecuted carries one transaction and its local replay result.
	TransactionExecuted Kind = "transaction_executed"
	// AccountUpdated carries one account's final rooted value for a slot.
	AccountUpdated Kind = "account_updated"
	// SlotRooted terminates a complete rooted slot.
	SlotRooted Kind = "slot_rooted"
)

// Cursor identifies one record in the rooted stream. Consumers resume after
// their last acknowledged cursor. Slot alone is insufficient because a slot
// can update many accounts.
type Cursor struct {
	Slot    uint64 `json:"slot"`
	Ordinal uint32 `json:"ordinal"`
}

// SlotMeta is the rooted identity supplied by replay for one SlotDelta.
type SlotMeta struct {
	Slot         uint64
	ParentSlot   uint64
	Bankhash     [32]byte
	Transactions []TransactionObservation
}

// CompiledInstruction preserves one instruction's canonical message indexes.
type CompiledInstruction struct {
	ProgramIDIndex uint16   `json:"program_id_index"`
	Accounts       []uint16 `json:"accounts"`
	Data           []byte   `json:"data"`
}

// InnerInstructions preserves the CPI trace for one top-level instruction.
type InnerInstructions struct {
	Index        uint8                 `json:"index"`
	Instructions []CompiledInstruction `json:"instructions"`
}

// ReturnData is the last program return value recorded during execution.
type ReturnData struct {
	ProgramID string `json:"program_id"`
	Data      []byte `json:"data"`
}

// TransactionObservation is replay's owned, fork-local input for one
// transaction event. Index must match the transaction's block order.
type TransactionObservation struct {
	Index         uint32
	Signature     string
	Message       []byte
	AccountKeys   []string
	Succeeded     bool
	Failure       string
	ComputeUnits  uint64
	Logs          []string
	LogsTruncated bool
	Inner         []InnerInstructions
	ReturnData    *ReturnData
}

// CloneTransactionObservations returns an owned deep copy safe to retain
// across asynchronous promotion or producer/replay handoff.
func CloneTransactionObservations(values []TransactionObservation) []TransactionObservation {
	out := append([]TransactionObservation(nil), values...)
	for i := range out {
		out[i].Message = append([]byte(nil), values[i].Message...)
		out[i].AccountKeys = append([]string(nil), values[i].AccountKeys...)
		out[i].Logs = append([]string(nil), values[i].Logs...)
		out[i].Inner = append([]InnerInstructions(nil), values[i].Inner...)
		for j := range out[i].Inner {
			out[i].Inner[j].Instructions = append([]CompiledInstruction(nil), values[i].Inner[j].Instructions...)
			for k := range out[i].Inner[j].Instructions {
				out[i].Inner[j].Instructions[k].Accounts = append([]uint16(nil), values[i].Inner[j].Instructions[k].Accounts...)
				out[i].Inner[j].Instructions[k].Data = append([]byte(nil), values[i].Inner[j].Instructions[k].Data...)
			}
		}
		if values[i].ReturnData != nil {
			value := *values[i].ReturnData
			value.Data = append([]byte(nil), values[i].ReturnData.Data...)
			out[i].ReturnData = &value
		}
	}
	return out
}

// TransactionRecord is one transaction and the result observed by local
// replay. Message is the canonical Solana message bytes; AccountKeys includes
// address-table-resolved keys for direct indexing.
type TransactionRecord struct {
	Index         uint32              `json:"index"`
	Signature     string              `json:"signature"`
	Message       []byte              `json:"message"`
	AccountKeys   []string            `json:"account_keys"`
	Succeeded     bool                `json:"succeeded"`
	Failure       string              `json:"failure,omitempty"`
	ComputeUnits  uint64              `json:"compute_units"`
	Logs          []string            `json:"logs,omitempty"`
	LogsTruncated bool                `json:"logs_truncated,omitempty"`
	Inner         []InnerInstructions `json:"inner_instructions,omitempty"`
	ReturnData    *ReturnData         `json:"return_data,omitempty"`
}

// AccountUpdate is one account's final value at Event.Cursor.Slot.
type AccountUpdate struct {
	Pubkey     string `json:"pubkey"`
	Owner      string `json:"owner"`
	Lamports   uint64 `json:"lamports"`
	Executable bool   `json:"executable"`
	RentEpoch  uint64 `json:"rent_epoch"`
	Data       []byte `json:"data"`
	Tombstone  bool   `json:"tombstone"`
}

// RootedSlot is the terminal lineage marker for one complete slot.
type RootedSlot struct {
	ParentSlot       uint64 `json:"parent_slot"`
	Bankhash         string `json:"bankhash"`
	TransactionCount uint32 `json:"transaction_count"`
	AccountCount     uint32 `json:"account_count"`
}

// Event is one transaction result, one final account value, or a rooted slot's
// terminal marker. Account updates are sorted by pubkey, so identical replay
// input always produces identical cursors and bytes.
type Event struct {
	SchemaVersion uint32             `json:"schema_version"`
	Cursor        Cursor             `json:"cursor"`
	Kind          Kind               `json:"kind"`
	Transaction   *TransactionRecord `json:"transaction,omitempty"`
	Account       *AccountUpdate     `json:"account,omitempty"`
	Root          *RootedSlot        `json:"root,omitempty"`
}

// BuildEvents converts ascending per-slot deltas into deterministic,
// owned events. The terminal SlotRooted event follows every transaction and
// account event for that slot and acknowledges a complete slot.
func BuildEvents(deltas []accounts.SlotDelta, metadata map[uint64]SlotMeta) ([]Event, error) {
	var events []Event
	err := walkEvents(deltas, metadata, func(event Event) error {
		if event.Transaction != nil {
			copy := cloneTransactionRecord(*event.Transaction)
			event.Transaction = &copy
		}
		if event.Account != nil {
			copy := *event.Account
			copy.Data = append([]byte(nil), event.Account.Data...)
			event.Account = &copy
		}
		events = append(events, event)
		return nil
	})
	return events, err
}

func walkEvents(deltas []accounts.SlotDelta, metadata map[uint64]SlotMeta, emit func(Event) error) error {
	for i, delta := range deltas {
		if i > 0 && delta.Slot <= deltas[i-1].Slot {
			return fmt.Errorf("rooted slots are not strictly ascending: %d follows %d", delta.Slot, deltas[i-1].Slot)
		}
		meta, ok := metadata[delta.Slot]
		if !ok {
			return fmt.Errorf("rooted slot %d has no metadata", delta.Slot)
		}
		if err := validateSlotMeta(meta, delta.Slot); err != nil {
			return err
		}
		if i > 0 && meta.ParentSlot != deltas[i-1].Slot {
			return fmt.Errorf("rooted slot %d parent is %d, want previous rooted slot %d", delta.Slot, meta.ParentSlot, deltas[i-1].Slot)
		}
		if err := validateEventCount(delta.Slot, uint64(len(meta.Transactions)), uint64(len(delta.Delta))); err != nil {
			return err
		}
		for index := range meta.Transactions {
			transaction := meta.Transactions[index]
			if err := validateTransaction(delta.Slot, uint32(index), transaction); err != nil {
				return err
			}
			record := cloneTransactionRecord(TransactionRecord{
				Index: transaction.Index, Signature: transaction.Signature,
				Message: transaction.Message, AccountKeys: transaction.AccountKeys,
				Succeeded: transaction.Succeeded, Failure: transaction.Failure,
				ComputeUnits: transaction.ComputeUnits, Logs: transaction.Logs,
				LogsTruncated: transaction.LogsTruncated,
				Inner:         transaction.Inner, ReturnData: transaction.ReturnData,
			})
			if err := emit(Event{
				SchemaVersion: SchemaVersion,
				Cursor:        Cursor{Slot: delta.Slot, Ordinal: uint32(index)},
				Kind:          TransactionExecuted,
				Transaction:   &record,
			}); err != nil {
				return err
			}
		}

		ordered := append([]*accounts.Account(nil), delta.Delta...)
		if err := validateAccounts(delta.Slot, ordered); err != nil {
			return err
		}
		sort.Slice(ordered, func(i, j int) bool {
			return bytes.Compare(ordered[i].Key[:], ordered[j].Key[:]) < 0
		})
		transactionCount := uint32(len(meta.Transactions))
		for ordinal, account := range ordered {
			owner := solana.PublicKey(account.Owner)
			if err := emit(Event{
				SchemaVersion: SchemaVersion,
				Cursor:        Cursor{Slot: delta.Slot, Ordinal: transactionCount + uint32(ordinal)},
				Kind:          AccountUpdated,
				Account: &AccountUpdate{
					Pubkey:     account.Key.String(),
					Owner:      owner.String(),
					Lamports:   account.Lamports,
					Executable: account.Executable,
					RentEpoch:  account.RentEpoch,
					Data:       account.Data,
					Tombstone:  account.Lamports == 0,
				},
			}); err != nil {
				return err
			}
		}
		if err := emit(Event{
			SchemaVersion: SchemaVersion,
			Cursor:        Cursor{Slot: delta.Slot, Ordinal: transactionCount + uint32(len(ordered))},
			Kind:          SlotRooted,
			Root: &RootedSlot{
				ParentSlot:       meta.ParentSlot,
				Bankhash:         solana.Hash(meta.Bankhash).String(),
				TransactionCount: transactionCount,
				AccountCount:     uint32(len(ordered)),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func validateEventCount(slot, transactions, accounts uint64) error {
	if transactions > math.MaxUint32 {
		return fmt.Errorf("rooted slot %d has too many transactions", slot)
	}
	if accounts > math.MaxUint32 {
		return fmt.Errorf("rooted slot %d has too many account updates", slot)
	}
	if transactions+accounts > math.MaxUint32 {
		return fmt.Errorf("rooted slot %d has too many transaction and account events", slot)
	}
	return nil
}

func validateTransaction(slot uint64, index uint32, transaction TransactionObservation) error {
	if transaction.Index != index {
		return fmt.Errorf("rooted slot %d transaction index %d identifies index %d", slot, index, transaction.Index)
	}
	if _, err := solana.SignatureFromBase58(transaction.Signature); err != nil {
		return fmt.Errorf("rooted slot %d transaction %d has invalid signature: %w", slot, index, err)
	}
	if len(transaction.Message) == 0 || len(transaction.Message) > maxTransactionMessageBytes {
		return fmt.Errorf("rooted slot %d transaction %d message size is outside allowed range 1..%d", slot, index, maxTransactionMessageBytes)
	}
	if len(transaction.AccountKeys) == 0 || len(transaction.AccountKeys) > maxTransactionAccountKeys {
		return fmt.Errorf("rooted slot %d transaction %d account count is outside allowed range 1..%d", slot, index, maxTransactionAccountKeys)
	}
	for accountIndex, key := range transaction.AccountKeys {
		if _, err := solana.PublicKeyFromBase58(key); err != nil {
			return fmt.Errorf("rooted slot %d transaction %d account %d is invalid: %w", slot, index, accountIndex, err)
		}
	}
	if transaction.Succeeded && transaction.Failure != "" {
		return fmt.Errorf("rooted slot %d transaction %d succeeded with failure %q", slot, index, transaction.Failure)
	}
	if !transaction.Succeeded && transaction.Failure == "" {
		return fmt.Errorf("rooted slot %d transaction %d failed without a failure class", slot, index)
	}
	if transaction.LogsTruncated &&
		(len(transaction.Logs) == 0 || transaction.Logs[len(transaction.Logs)-1] != "Log truncated") {
		return fmt.Errorf("rooted slot %d transaction %d has malformed truncated logs", slot, index)
	}
	var logBytes uint64
	for _, log := range transaction.Logs {
		logBytes += uint64(len(log))
		if logBytes > maxTransactionLogBytes {
			return fmt.Errorf("rooted slot %d transaction %d logs exceed %d bytes", slot, index, maxTransactionLogBytes)
		}
	}
	innerCount := 0
	var lastGroup uint8
	for groupIndex, group := range transaction.Inner {
		if groupIndex > 0 && group.Index <= lastGroup {
			return fmt.Errorf("rooted slot %d transaction %d inner-instruction groups are not ordered", slot, index)
		}
		lastGroup = group.Index
		for _, instruction := range group.Instructions {
			innerCount++
			if innerCount > maxInnerInstructions || int(instruction.ProgramIDIndex) >= len(transaction.AccountKeys) ||
				len(instruction.Accounts) > maxInnerInstructionAccounts || len(instruction.Data) > maxInnerInstructionDataBytes {
				return fmt.Errorf("rooted slot %d transaction %d inner instruction exceeds runtime bounds", slot, index)
			}
			for _, account := range instruction.Accounts {
				if int(account) >= len(transaction.AccountKeys) {
					return fmt.Errorf("rooted slot %d transaction %d inner-instruction account index is invalid", slot, index)
				}
			}
		}
	}
	if transaction.ReturnData != nil {
		if _, err := solana.PublicKeyFromBase58(transaction.ReturnData.ProgramID); err != nil {
			return fmt.Errorf("rooted slot %d transaction %d return program is invalid: %w", slot, index, err)
		}
		if len(transaction.ReturnData.Data) > maxReturnDataBytes {
			return fmt.Errorf("rooted slot %d transaction %d return data exceeds %d bytes", slot, index, maxReturnDataBytes)
		}
	}
	return nil
}

func cloneTransactionRecord(record TransactionRecord) TransactionRecord {
	record.Message = append([]byte(nil), record.Message...)
	record.AccountKeys = append([]string(nil), record.AccountKeys...)
	record.Logs = append([]string(nil), record.Logs...)
	record.Inner = append([]InnerInstructions(nil), record.Inner...)
	for i := range record.Inner {
		record.Inner[i].Instructions = append([]CompiledInstruction(nil), record.Inner[i].Instructions...)
		for j := range record.Inner[i].Instructions {
			record.Inner[i].Instructions[j].Accounts = append([]uint16(nil), record.Inner[i].Instructions[j].Accounts...)
			record.Inner[i].Instructions[j].Data = append([]byte(nil), record.Inner[i].Instructions[j].Data...)
		}
	}
	if record.ReturnData != nil {
		value := *record.ReturnData
		value.Data = append([]byte(nil), value.Data...)
		record.ReturnData = &value
	}
	return record
}

func validateSlotMeta(meta SlotMeta, slot uint64) error {
	if meta.Slot != slot {
		return fmt.Errorf("rooted slot %d metadata identifies slot %d", slot, meta.Slot)
	}
	if slot == 0 {
		if meta.ParentSlot != 0 {
			return fmt.Errorf("genesis slot parent is %d, want 0", meta.ParentSlot)
		}
	} else if meta.ParentSlot >= slot {
		return fmt.Errorf("rooted slot %d has invalid parent %d", slot, meta.ParentSlot)
	}
	if meta.Bankhash == ([32]byte{}) {
		return fmt.Errorf("rooted slot %d has an empty bankhash", slot)
	}
	return nil
}

func validateAccounts(slot uint64, values []*accounts.Account) error {
	seen := make(map[solana.PublicKey]struct{}, len(values))
	for _, account := range values {
		if account == nil {
			return fmt.Errorf("rooted slot %d contains a nil account update", slot)
		}
		if len(account.Data) > maxAccountDataBytes {
			return fmt.Errorf("rooted slot %d account %s data exceeds %d bytes", slot, account.Key, maxAccountDataBytes)
		}
		if _, duplicate := seen[account.Key]; duplicate {
			return fmt.Errorf("rooted slot %d contains duplicate account %s", slot, account.Key)
		}
		seen[account.Key] = struct{}{}
	}
	return nil
}

// ErrCursorNotFound means the requested acknowledgement is outside this batch.
var ErrCursorNotFound = errors.New("rooted event cursor not found")

// EventsAfter returns the records strictly after cursor. It rejects an unknown
// cursor rather than silently skipping data. A zero value cursor is not a
// special case; callers start from the beginning by passing nil.
func EventsAfter(events []Event, cursor *Cursor) ([]Event, error) {
	if cursor == nil {
		return events, nil
	}
	for i := range events {
		if events[i].Cursor == *cursor {
			return events[i+1:], nil
		}
	}
	return nil, ErrCursorNotFound
}

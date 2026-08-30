package block

import (
	"errors"
	"fmt"
	"math"

	"github.com/Overclock-Validator/mithril/pkg/overcast"
	"github.com/gagliardetto/solana-go"
)

const maxLegacyTransactionBytes = 1232

// FromLightbringerStreamMsg converts a trusted Lightbringer response.
// Network consumers should use DecodeLightbringerStreamMsg to handle invalid input.
func FromLightbringerStreamMsg(resp *overcast.SlotResponse) *Block {
	block, err := DecodeLightbringerStreamMsg(resp)
	if err != nil {
		return nil
	}
	return block
}

// DecodeLightbringerStreamMsg validates and converts a Lightbringer response.
func DecodeLightbringerStreamMsg(resp *overcast.SlotResponse) (*Block, error) {
	if resp == nil {
		return nil, errors.New("Lightbringer slot response is nil")
	}
	if len(resp.Entries) == 0 {
		return nil, fmt.Errorf("Lightbringer slot %d has no entries", resp.Slot)
	}

	block := new(Block)
	block.Slot = resp.Slot
	block.SourceParentSlot = resp.GetParentSlot()
	block.Transactions = make([]*solana.Transaction, 0, 2000)

	for entryIndex, entry := range resp.Entries {
		if entry == nil {
			return nil, fmt.Errorf("Lightbringer slot %d entry %d is nil", resp.Slot, entryIndex)
		}
		if len(entry.Hash) != len(solana.Hash{}) {
			return nil, fmt.Errorf("Lightbringer slot %d entry %d hash length is %d, want %d", resp.Slot, entryIndex, len(entry.Hash), len(solana.Hash{}))
		}
		for transactionIndex, tx := range entry.Transactions {
			convertedTx, txVersion, err := overcastTransactionToTransaction(tx)
			if err != nil {
				return nil, fmt.Errorf("Lightbringer slot %d entry %d transaction %d: %w", resp.Slot, entryIndex, transactionIndex, err)
			}
			block.Transactions = append(block.Transactions, convertedTx)
			block.NumSignatures += uint64(convertedTx.Message.Header.NumRequiredSignatures)
			block.Versions = append(block.Versions, txVersion)
		}
	}

	var offset uint64
	block.Entries = make([]*TxEntry, len(resp.Entries))

	for idx, entry := range resp.Entries {
		numTransactionsInEntry := uint64(len(entry.Transactions))
		txEntry := &TxEntry{NumHashes: entry.NumHashes,
			Hash:    entry.Hash,
			Indices: make([]uint64, len(entry.Transactions))}

		for j := range numTransactionsInEntry {
			txEntry.Indices[j] = offset + j
		}

		block.Entries[idx] = txEntry
		offset += numTransactionsInEntry
	}

	block.Blockhash = solana.HashFromBytes(resp.Entries[len(resp.Entries)-1].Hash[:])
	block.FromLiveStream = true

	return block, nil
}

func overcastTransactionToTransaction(overcastTx *overcast.VersionedTransaction) (*solana.Transaction, uint8, error) {
	if overcastTx == nil {
		return nil, 0, errors.New("transaction is nil")
	}

	tx := &solana.Transaction{}

	for signatureIndex, s := range overcastTx.Signatures {
		if len(s) != len(solana.Signature{}) {
			return nil, 0, fmt.Errorf("signature %d length is %d, want %d", signatureIndex, len(s), len(solana.Signature{}))
		}
		convertedSig := solana.SignatureFromBytes(s)
		tx.Signatures = append(tx.Signatures, convertedSig)
	}

	var (
		header          *overcast.MessageHeader
		acctKeys        [][]byte
		recentBlockhash []byte
		instrs          []*overcast.CompiledInstruction
		lookups         []*overcast.MessageAddressTableLookup
		config          solana.TransactionConfig
		version         = solana.MessageVersionLegacy
	)
	switch message := overcastTx.GetMessage().(type) {
	case *overcast.VersionedTransaction_MessageLegacy:
		header = message.MessageLegacy.GetHeader()
		acctKeys = message.MessageLegacy.GetAccountKeys()
		recentBlockhash = message.MessageLegacy.GetRecentBlockhash()
		instrs = message.MessageLegacy.GetInstructions()
	case *overcast.VersionedTransaction_MessageV0:
		header = message.MessageV0.GetHeader()
		acctKeys = message.MessageV0.GetAccountKeys()
		recentBlockhash = message.MessageV0.GetRecentBlockhash()
		instrs = message.MessageV0.GetInstructions()
		lookups = message.MessageV0.GetAddressTableLookups()
		version = solana.MessageVersionV0
	case *overcast.VersionedTransaction_MessageV1:
		header = message.MessageV1.GetHeader()
		acctKeys = message.MessageV1.GetAccountKeys()
		recentBlockhash = message.MessageV1.GetLifetimeSpecifier()
		instrs = message.MessageV1.GetInstructions()
		if message.MessageV1.Config != nil {
			config = solana.TransactionConfig{
				PriorityFee:                 message.MessageV1.Config.PriorityFee,
				ComputeUnitLimit:            message.MessageV1.Config.ComputeUnitLimit,
				LoadedAccountsDataSizeLimit: message.MessageV1.Config.LoadedAccountsDataSizeLimit,
				HeapSize:                    message.MessageV1.Config.HeapSize,
			}
		}
		version = solana.MessageVersionV1
	default:
		return nil, 0, errors.New("transaction message is missing")
	}
	if header == nil {
		return nil, 0, errors.New("transaction message header is missing")
	}
	if header.NumReadonlySignedAccounts > math.MaxUint8 ||
		header.NumReadonlyUnsignedAccounts > math.MaxUint8 ||
		header.NumRequiredSignatures > math.MaxUint8 {
		return nil, 0, errors.New("transaction message header exceeds uint8")
	}
	if len(recentBlockhash) != len(solana.Hash{}) {
		return nil, 0, fmt.Errorf("transaction lifetime hash length is %d, want %d", len(recentBlockhash), len(solana.Hash{}))
	}

	for accountIndex, acctKey := range acctKeys {
		if len(acctKey) != len(solana.PublicKey{}) {
			return nil, 0, fmt.Errorf("account key %d length is %d, want %d", accountIndex, len(acctKey), len(solana.PublicKey{}))
		}
		convertedAcctKey := solana.PublicKeyFromBytes(acctKey)
		tx.Message.AccountKeys = append(tx.Message.AccountKeys, convertedAcctKey)
	}

	tx.Message.Header.NumReadonlySignedAccounts = uint8(header.NumReadonlySignedAccounts)
	tx.Message.Header.NumReadonlyUnsignedAccounts = uint8(header.NumReadonlyUnsignedAccounts)
	tx.Message.Header.NumRequiredSignatures = uint8(header.NumRequiredSignatures)
	tx.Message.RecentBlockhash = solana.HashFromBytes(recentBlockhash)

	for instructionIndex, instr := range instrs {
		if instr == nil {
			return nil, 0, fmt.Errorf("instruction %d is nil", instructionIndex)
		}
		if instr.ProgramIdIndex > math.MaxUint8 {
			return nil, 0, fmt.Errorf("instruction %d program id index exceeds uint8", instructionIndex)
		}
		convertedInstr := overcastInstrToInstr(instr)
		tx.Message.Instructions = append(tx.Message.Instructions, convertedInstr)
	}

	if version == solana.MessageVersionV0 {
		convertedLookups := make([]solana.MessageAddressTableLookup, len(lookups))
		for idx, addrTableLookup := range lookups {
			if addrTableLookup == nil {
				return nil, 0, fmt.Errorf("address table lookup %d is nil", idx)
			}
			if len(addrTableLookup.AccountKey) != len(solana.PublicKey{}) {
				return nil, 0, fmt.Errorf("address table lookup %d account key length is %d, want %d", idx, len(addrTableLookup.AccountKey), len(solana.PublicKey{}))
			}
			convertedLookups[idx] = overcastAddrTableLookupToAddrTableLookup(addrTableLookup)
		}
		tx.Message.SetAddressTableLookups(convertedLookups)
	}
	tx.Message.TransactionConfig = config
	if _, err := tx.Message.SetVersion(version); err != nil {
		return nil, 0, fmt.Errorf("set transaction version %d: %w", version, err)
	}
	if err := tx.Sanitize(); err != nil {
		return nil, 0, fmt.Errorf("sanitize transaction version %d: %w", version, err)
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, 0, fmt.Errorf("marshal transaction version %d: %w", version, err)
	}
	maxBytes := maxLegacyTransactionBytes
	if version == solana.MessageVersionV1 {
		maxBytes = solana.MaxTransactionSizeV1
	}
	if len(wire) > maxBytes {
		return nil, 0, fmt.Errorf("transaction version %d size is %d, maximum is %d", version, len(wire), maxBytes)
	}

	return tx, uint8(version), nil
}

func overcastInstrToInstr(instr *overcast.CompiledInstruction) solana.CompiledInstruction {
	compiledInstr := solana.CompiledInstruction{}
	compiledInstr.ProgramIDIndex = uint16(instr.ProgramIdIndex)

	for _, acct := range instr.Accounts {
		compiledInstr.Accounts = append(compiledInstr.Accounts, uint16(acct))
	}

	compiledInstr.Data = instr.Data
	return compiledInstr
}

func overcastAddrTableLookupToAddrTableLookup(atl *overcast.MessageAddressTableLookup) solana.MessageAddressTableLookup {
	converted := solana.MessageAddressTableLookup{}
	converted.AccountKey = solana.PublicKeyFromBytes(atl.AccountKey)
	converted.ReadonlyIndexes = atl.ReadonlyIndexes
	converted.WritableIndexes = atl.WritableIndexes
	return converted
}

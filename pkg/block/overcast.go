package block

import (
	"github.com/Overclock-Validator/mithril/pkg/global"
	"github.com/Overclock-Validator/mithril/pkg/overcast"
	"github.com/gagliardetto/solana-go"
)

func FromOvercastMsg(resp *overcast.SlotResponse) *Block {
	block := new(Block)
	block.Slot = resp.Slot
	block.Transactions = make([]*solana.Transaction, 0, 2000)

	for _, entry := range resp.Entries {
		for _, tx := range entry.Transactions {
			convertedTx, txVersion := overcastTransactionToTransaction(tx)
			block.Transactions = append(block.Transactions, convertedTx)
			block.NumSignatures += uint64(convertedTx.Message.Header.NumRequiredSignatures)
			block.Versions = append(block.Versions, txVersion)
		}
	}

	block.Entries = resp.Entries
	block.Blockhash = solana.HashFromBytes(resp.Entries[len(resp.Entries)-1].Hash[:])
	block.BlockHeight = global.BlockHeight()

	return block
}

func overcastTransactionToTransaction(overcastTx *overcast.VersionedTransaction) (*solana.Transaction, uint8) {
	tx := &solana.Transaction{}

	for _, s := range overcastTx.Signatures {
		convertedSig := solana.SignatureFromBytes(s)
		tx.Signatures = append(tx.Signatures, convertedSig)
	}

	isV0 := overcastTx.GetMessageV0() != nil

	var acctKeys [][]byte
	if isV0 {
		acctKeys = overcastTx.GetMessageV0().AccountKeys
	} else {
		acctKeys = overcastTx.GetMessageLegacy().AccountKeys
	}

	for _, acctKey := range acctKeys {
		convertedAcctKey := solana.PublicKeyFromBytes(acctKey)
		tx.Message.AccountKeys = append(tx.Message.AccountKeys, convertedAcctKey)
	}

	if isV0 {
		tx.Message.Header.NumReadonlySignedAccounts = uint8(overcastTx.GetMessageV0().Header.NumReadonlySignedAccounts)
		tx.Message.Header.NumReadonlyUnsignedAccounts = uint8(overcastTx.GetMessageV0().Header.NumReadonlyUnsignedAccounts)
		tx.Message.Header.NumRequiredSignatures = uint8(overcastTx.GetMessageV0().Header.NumRequiredSignatures)
	} else {
		tx.Message.Header.NumReadonlySignedAccounts = uint8(overcastTx.GetMessageLegacy().Header.NumReadonlySignedAccounts)
		tx.Message.Header.NumReadonlyUnsignedAccounts = uint8(overcastTx.GetMessageLegacy().Header.NumReadonlyUnsignedAccounts)
		tx.Message.Header.NumRequiredSignatures = uint8(overcastTx.GetMessageLegacy().Header.NumRequiredSignatures)
	}

	if isV0 {
		tx.Message.RecentBlockhash = solana.Hash(overcastTx.GetMessageV0().RecentBlockhash)
	} else {
		tx.Message.RecentBlockhash = solana.Hash(overcastTx.GetMessageLegacy().RecentBlockhash)
	}

	var instrs []*overcast.CompiledInstruction
	if isV0 {
		instrs = overcastTx.GetMessageV0().Instructions
	} else {
		instrs = overcastTx.GetMessageLegacy().Instructions
	}

	for _, instr := range instrs {
		convertedInstr := overcastInstrToInstr(instr)
		tx.Message.Instructions = append(tx.Message.Instructions, convertedInstr)
	}

	if isV0 {
		for _, addrTableLookup := range overcastTx.GetMessageV0().AddressTableLookups {
			convertedAtl := overcastAddrTableLookupToAddrTableLookup(addrTableLookup)
			tx.Message.AddressTableLookups = append(tx.Message.AddressTableLookups, convertedAtl)
		}
	}

	var version uint8
	if isV0 {
		version = 1
	}

	return tx, version
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

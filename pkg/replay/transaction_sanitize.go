package replay

import (
	"fmt"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/gagliardetto/solana-go"
)

const (
	legacyMaxTransactionAccountLocks = 64
	maxTransactionAccountLocks       = 128
)

// ValidateTransactionShape applies the bank-independent transaction and
// message checks before account loading. It works both before and after v0
// lookup resolution; program IDs must always remain in the static key set.
func ValidateTransactionShape(tx *solana.Transaction, feats *features.Features) error {
	if tx == nil {
		return fmt.Errorf("%w: nil transaction", TxErrSanitizeFailure)
	}
	if tx.Message.GetVersion() == solana.MessageVersionV1 {
		if feats != nil && !feats.IsActive(features.EnableTransactionV1) {
			return fmt.Errorf("%w: transaction v1 is not active for this bank", TxErrSanitizeFailure)
		}
		if err := tx.Sanitize(); err != nil {
			return fmt.Errorf("%w: %v", TxErrSanitizeFailure, err)
		}
		wire, err := tx.MarshalBinary()
		if err != nil || len(wire) > solana.MaxTransactionSizeV1 {
			return fmt.Errorf("%w: v1 transaction exceeds %d bytes", TxErrSanitizeFailure, solana.MaxTransactionSizeV1)
		}
	}
	dynamicKeys := tx.Message.AddressTableLookups.NumLookups()
	staticKeys := len(tx.Message.AccountKeys)
	if tx.Message.IsResolved() {
		if dynamicKeys > staticKeys {
			return fmt.Errorf("%w: resolved lookup count exceeds account keys", TxErrSanitizeFailure)
		}
		staticKeys -= dynamicKeys
	}
	totalKeys := staticKeys + dynamicKeys
	hdr := tx.Message.Header
	required := int(hdr.NumRequiredSignatures)
	if required == 0 || len(tx.Signatures) != required || required > staticKeys ||
		int(hdr.NumReadonlySignedAccounts) >= required ||
		required+int(hdr.NumReadonlyUnsignedAccounts) > staticKeys || totalKeys > 256 {
		return fmt.Errorf("%w: invalid message header or account count", TxErrSanitizeFailure)
	}
	if tx.Message.IsVersioned() {
		for _, lookup := range tx.Message.AddressTableLookups {
			if len(lookup.WritableIndexes)+len(lookup.ReadonlyIndexes) == 0 {
				return fmt.Errorf("%w: empty address table lookup", TxErrSanitizeFailure)
			}
		}
	}
	if feats != nil && feats.IsActive(features.StaticInstructionLimit) && len(tx.Message.Instructions) > maxInstrTraceCapacity {
		return fmt.Errorf("%w: too many instructions", TxErrSanitizeFailure)
	}
	for _, instruction := range tx.Message.Instructions {
		programIndex := int(instruction.ProgramIDIndex)
		if programIndex == 0 || programIndex >= staticKeys {
			return fmt.Errorf("%w: invalid program index", TxErrSanitizeFailure)
		}
		for _, accountIndex := range instruction.Accounts {
			if int(accountIndex) >= totalKeys {
				return fmt.Errorf("%w: invalid account index", TxErrSanitizeFailure)
			}
		}
	}
	return nil
}

func transactionAccountLockError(tx *solana.Transaction, feats *features.Features) (TransactionErrorType, bool) {
	seen := make(map[solana.PublicKey]struct{}, len(tx.Message.AccountKeys))
	for _, key := range tx.Message.AccountKeys {
		if _, ok := seen[key]; ok {
			return TransactionErrorAccountLoadedTwice, true
		}
		seen[key] = struct{}{}
	}
	limit := legacyMaxTransactionAccountLocks
	if feats != nil && feats.IsActive(features.IncreaseTxAccountLockLimit) {
		limit = maxTransactionAccountLocks
	}
	if len(tx.Message.AccountKeys) > limit {
		return TransactionErrorTooManyAccountLocks, true
	}
	return 0, false
}

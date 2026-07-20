package replay

import (
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/zeebo/blake3"
)

const transactionMessageHashDomain = "solana-tx-message-v1"

const maxDuplicateTransactionOccurrences = 16

// DuplicateTransactionOccurrence identifies a repeated transaction message
// and the earlier transaction whose message hash it duplicates.
type DuplicateTransactionOccurrence struct {
	Index      int
	FirstIndex int
}

// DuplicateTransactionMessagesError means a block is invalid under Agave's
// AlreadyProcessed semantics. Occurrences is a bounded diagnostic sample;
// DuplicateCount always contains the complete count.
type DuplicateTransactionMessagesError struct {
	Slot           uint64
	DuplicateCount uint64
	Occurrences    []DuplicateTransactionOccurrence
}

func (e *DuplicateTransactionMessagesError) Error() string {
	if e == nil {
		return "duplicate transaction messages (AlreadyProcessed)"
	}
	var details strings.Builder
	for i, occurrence := range e.Occurrences {
		if i > 0 {
			details.WriteString(", ")
		}
		_, _ = fmt.Fprintf(&details, "%d->%d", occurrence.Index, occurrence.FirstIndex)
	}
	slot := ""
	if e.Slot != 0 {
		slot = fmt.Sprintf("slot %d ", e.Slot)
	}
	if details.Len() == 0 {
		return fmt.Sprintf("%scontains %d duplicate transaction messages (AlreadyProcessed)", slot, e.DuplicateCount)
	}
	suffix := ""
	if uint64(len(e.Occurrences)) < e.DuplicateCount {
		suffix = fmt.Sprintf(" (showing first %d)", len(e.Occurrences))
	}
	return fmt.Sprintf("%scontains %d duplicate transaction messages (AlreadyProcessed); duplicate indexes (duplicate->first): %s%s",
		slot, e.DuplicateCount, details.String(), suffix)
}

// TransactionMessageHash returns the message identity Agave uses for
// AlreadyProcessed checks. Signatures are deliberately excluded.
func TransactionMessageHash(tx *solana.Transaction) ([32]byte, error) {
	var messageHash [32]byte
	if tx == nil {
		return messageHash, fmt.Errorf("transaction is nil")
	}
	message, err := tx.Message.MarshalBinary()
	if err != nil {
		return messageHash, fmt.Errorf("serialize transaction message: %w", err)
	}

	hasher := blake3.New()
	_, _ = hasher.Write([]byte(transactionMessageHashDomain))
	_, _ = hasher.Write(message)
	copy(messageHash[:], hasher.Sum(nil))
	return messageHash, nil
}

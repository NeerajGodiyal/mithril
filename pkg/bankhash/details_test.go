package bankhash

import (
	"encoding/json"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accounts"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestBuildSlotDetailsMatchesAgaveShape(t *testing.T) {
	acct := &accounts.Account{
		Key:      solana.MustPublicKeyFromBase58("11111111111111111111111111111111"),
		Owner:    solana.MustPublicKeyFromBase58("11111111111111111111111111111112"),
		Lamports: 42,
		Data:     []byte{1, 2, 3},
	}
	parent := solana.Hash{1}
	entry := solana.Hash{2}
	bankHash := make([]byte, 32)
	bankHash[0] = 9
	producerNanos := uint64(123456789)
	commitSec := int64(1700000000)

	details := BuildSlotDetails(SlotDetailsInput{
		Slot:                    3686504,
		BankHash:                bankHash,
		ParentBankHash:          parent,
		SignatureCount:          0,
		LastBlockhash:           entry,
		Accounts:                []*accounts.Account{acct},
		FooterProducerTimeNanos: &producerNanos,
		CommitFooterTimestamp:   &commitSec,
	})

	raw, err := json.Marshal(details)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, float64(3686504), decoded["slot"])
	require.NotEmpty(t, decoded["bank_hash"])
	require.NotEmpty(t, decoded["parent_bank_hash"])
	require.Equal(t, float64(0), decoded["signature_count"])
	require.NotEmpty(t, decoded["last_blockhash"])
	require.Equal(t, float64(123456789), decoded["block_producer_time_nanos"])
	require.Equal(t, float64(1700000000), decoded["commit_footer_timestamp_seconds"])
	accountsField := decoded["accounts"].([]any)
	require.Len(t, accountsField, 1)
}

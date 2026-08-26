package wire

import (
	stded25519 "crypto/ed25519"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/txverify"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/require"
)

func signedV0Wire(t *testing.T) []byte {
	t.Helper()
	payer, err := solana.NewRandomPrivateKey()
	require.NoError(t, err)
	tx, err := solana.NewTransaction(
		[]solana.Instruction{system.NewTransferInstruction(1, payer.PublicKey(), payer.PublicKey()).Build()},
		solana.Hash{},
		solana.TransactionPayer(payer.PublicKey()),
	)
	require.NoError(t, err)
	tx.Message.SetVersion(solana.MessageVersionV0)
	message, err := txverify.MessageBytes(tx)
	require.NoError(t, err)
	signature := stded25519.Sign(stded25519.PrivateKey(payer), message)
	wire := make([]byte, 1, 1+len(signature)+len(message))
	wire[0] = 1
	wire = append(wire, signature...)
	wire = append(wire, message...)
	return wire
}

func TestSanitizeAcceptsV0Wire(t *testing.T) {
	wire := signedV0Wire(t)
	view, err := Sanitize(wire)
	require.NoError(t, err)
	require.Equal(t, byte(0x80), view.Message()[0])
	require.Len(t, view.FirstSignature(), solana.SignatureLength)
}

func TestSanitizeRejectsUnsupportedMessageVersion(t *testing.T) {
	wire := signedV0Wire(t)
	wire[1+solana.SignatureLength] = 0x81
	_, err := Sanitize(wire)
	require.ErrorIs(t, err, ErrInvalidMessage)
}

func TestSanitizeAcceptsV1WireWithTrailingSignatures(t *testing.T) {
	payer := solana.PrivateKey(stded25519.NewKeyFromSeed(make([]byte, 32)))
	tx, err := solana.NewTransaction(
		[]solana.Instruction{system.NewTransferInstruction(1, payer.PublicKey(), payer.PublicKey()).Build()},
		solana.Hash{},
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionV1Config(solana.TransactionConfig{}.WithComputeUnitLimit(20_000)),
	)
	require.NoError(t, err)
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key == payer.PublicKey() {
			return &payer
		}
		return nil
	})
	require.NoError(t, err)
	wire, err := tx.MarshalBinary()
	require.NoError(t, err)

	view, err := Sanitize(wire)
	require.NoError(t, err)
	require.Equal(t, byte(0x81), view.Message()[0])
	require.Equal(t, tx.Signatures[0][:], view.FirstSignature())
}
